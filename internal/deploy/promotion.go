package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/secrets"
	"github.com/tricknotes/koroku/internal/store"
)

// Reasons a built service of a promotion is built instead of reusing the
// source release's image (ARCHITECTURE 9.7). They are part of the API.
const (
	ReasonArgsDiffer       = "args_differ"
	ReasonBuildDiffers     = "build_differs"
	ReasonNotBuiltInSource = "not_built_in_source"
	ReasonImageMissing     = "image_missing"
	ReasonSourceMissing    = "source_missing"
)

// ImageDecision says whether a built service of a promotion reuses the
// image of the promoted release. It never carries values of variables.
type ImageDecision struct {
	Service string
	Reuse   bool
	// Reason is one of the Reason* constants when Reuse is false.
	Reason string
	// DifferingArgs are the build.args keys that differ (ReasonArgsDiffer).
	DifferingArgs []string
	// SourceImage is the image of the promoted release (when Reuse).
	SourceImage string
}

// Explain describes why the service is built, for the deploy log.
func (d ImageDecision) Explain() string {
	switch d.Reason {
	case ReasonArgsDiffer:
		return "build args differ: " + strings.Join(d.DifferingArgs, ", ")
	case ReasonBuildDiffers:
		return "the build definition differs"
	case ReasonNotBuiltInSource:
		return "the promoted release does not build this service"
	case ReasonImageMissing:
		return "the image of the promoted release is gone"
	case ReasonSourceMissing:
		return "the promoted release is no longer on disk"
	default:
		return d.Reason
	}
}

// ErrSourceReleaseMissing is returned by PreviewPromotionImages when the
// promoted release's files are gone.
var ErrSourceReleaseMissing = errors.New("the promoted release is no longer on disk")

// EnqueuePromotion queues a deployment of source's commit to app, recorded
// as a promotion of source (ARCHITECTURE 9.7). The caller checks that the
// apps are paired and that source is the source app's current release.
func (e *Engine) EnqueuePromotion(ctx context.Context, app store.App, source store.Deployment) (store.Deployment, error) {
	return e.enqueue(ctx, app, "", source.CommitSHA, source.CommitMessage, &source.ID)
}

// sourceProject reads the normalized configuration of a stored release's
// repository Compose file (without the override), interpolated with the
// env the release ran with.
func (e *Engine) sourceProject(ctx context.Context, src store.App, id int64) (compose.Project, string, string, error) {
	meta, err := e.readMeta(src.Slug, id)
	if err != nil {
		return compose.Project{}, "", "", ErrSourceReleaseMissing
	}
	dir := e.ReleaseDir(src.Slug, id)
	snapshot := filepath.Join(e.stateDir(src.Slug, id), "env")
	for _, path := range []string{filepath.Join(dir, meta.ComposeFile), snapshot} {
		if _, err := os.Stat(path); err != nil {
			return compose.Project{}, "", "", ErrSourceReleaseMissing
		}
	}
	inv := compose.Invocation{
		ProjectName: src.ProjectName(),
		Files:       []string{filepath.Join(dir, meta.ComposeFile)},
		EnvFile:     snapshot,
	}
	project, err := e.composeConfigTo(ctx, inv, dir, io.Discard)
	if err != nil {
		return compose.Project{}, "", "", fmt.Errorf("read the promoted release: %w", err)
	}
	return project, dir, meta.ComposeFile, nil
}

// decideImages compares every built service of target (the repository
// Compose file of the new release, read with the target app's env, in
// targetDir) with the promoted release.
func (e *Engine) decideImages(ctx context.Context, target compose.Project, targetDir string,
	source compose.Project, sourceDir string, src store.App, srcID int64,
) []ImageDecision {
	var out []ImageDecision
	for _, name := range target.ServiceNames() {
		svc := target.Services[name]
		if !svc.HasBuild() {
			continue
		}
		d := ImageDecision{Service: name}
		srcSvc, ok := source.Services[name]
		if !ok || !srcSvc.HasBuild() {
			d.Reason = ReasonNotBuiltInSource
			out = append(out, d)
			continue
		}
		diff, err := compose.CompareBuilds(srcSvc.Build, sourceDir, svc.Build, targetDir)
		switch {
		case err != nil || diff.OtherDiffers:
			d.Reason = ReasonBuildDiffers
		case len(diff.DifferingArgs) > 0:
			d.Reason, d.DifferingArgs = ReasonArgsDiffer, diff.DifferingArgs
		default:
			image := compose.ReleaseImage(src.ProjectName(), name, "r"+strconv.FormatInt(srcID, 10))
			if e.imageExists(ctx, image) {
				d.Reuse, d.SourceImage = true, image
			} else {
				d.Reason = ReasonImageMissing
			}
		}
		out = append(out, d)
	}
	return out
}

func (e *Engine) imageExists(ctx context.Context, image string) bool {
	return e.opts.Runner.Run(ctx, runner.Cmd{
		Name:   "docker",
		Args:   []string{"image", "inspect", "--format", "{{.Id}}", image},
		Stdout: io.Discard,
		Stderr: io.Discard,
	}) == nil
}

// PreviewPromotionImages tells, before a promotion is queued, which built
// services would reuse the images of source's release. It reads the
// release's checkout (the commit that would be deployed) with the target
// app's current variables, like the deployment will. The variables are
// written to a temporary file in the target app's directory, outside any
// build context, and removed afterwards.
func (e *Engine) PreviewPromotionImages(ctx context.Context, target, src store.App, source store.Deployment) ([]ImageDecision, error) {
	sourceProject, dir, _, err := e.sourceProject(ctx, src, source.ID)
	if err != nil {
		return nil, err
	}
	composeFile, err := compose.DetectFile(dir, target.ComposePath)
	if err != nil {
		return nil, err
	}
	content, _, err := e.renderEnv(ctx, target, source.CommitSHA, secrets.NewMasker())
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(e.appDir(target.Slug), 0o700); err != nil {
		return nil, fmt.Errorf("prepare app directory: %w", err)
	}
	tmp, err := os.CreateTemp(e.appDir(target.Slug), ".promotion-env-*")
	if err != nil {
		return nil, fmt.Errorf("write env for the preview: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, writeErr := tmp.WriteString(content)
	if err := errors.Join(writeErr, tmp.Close()); err != nil {
		return nil, fmt.Errorf("write env for the preview: %w", err)
	}
	inv := compose.Invocation{
		ProjectName: target.ProjectName(),
		Files:       []string{filepath.Join(dir, composeFile)},
		EnvFile:     tmp.Name(),
	}
	targetProject, err := e.composeConfigTo(ctx, inv, dir, io.Discard)
	if err != nil {
		return nil, err
	}
	return e.decideImages(ctx, targetProject, dir, sourceProject, dir, src, source.ID), nil
}

// reuseImages runs the reuse step of a promotion: it decides per built
// service, tags the reusable images with the new release's names and
// returns the services that still need a build.
func (e *Engine) reuseImages(ctx context.Context, d store.Deployment, app store.App,
	repoProject compose.Project, releaseDir string, log *deployLog,
) []string {
	var built []string
	for _, name := range repoProject.ServiceNames() {
		if repoProject.Services[name].HasBuild() {
			built = append(built, name)
		}
	}
	srcDeployment, err := e.opts.Store.GetDeployment(ctx, *d.PromotedFromID)
	if err != nil {
		log.Printf("The promoted deployment #%d no longer exists; building all images.", *d.PromotedFromID)
		return built
	}
	src, err := e.opts.Store.GetAppByID(ctx, srcDeployment.AppID)
	if err != nil {
		log.Printf("The app of the promoted deployment no longer exists; building all images.")
		return built
	}
	log.Step(StepReuse, "Reusing images of %s deployment #%d", src.Slug, srcDeployment.ID)

	var decisions []ImageDecision
	sourceProject, sourceDir, _, err := e.sourceProject(ctx, src, srcDeployment.ID)
	if err != nil {
		reason := ReasonSourceMissing
		if !errors.Is(err, ErrSourceReleaseMissing) {
			log.Printf("Reading the promoted release failed: %v", err)
			reason = ReasonBuildDiffers
		}
		for _, name := range built {
			decisions = append(decisions, ImageDecision{Service: name, Reason: reason})
		}
	} else {
		decisions = e.decideImages(ctx, repoProject, releaseDir, sourceProject, sourceDir, src, srcDeployment.ID)
	}

	tag := "r" + strconv.FormatInt(d.ID, 10)
	var rest []string
	for _, decision := range decisions {
		if !decision.Reuse {
			log.Printf("%s: building (%s)", decision.Service, decision.Explain())
			rest = append(rest, decision.Service)
			continue
		}
		target := compose.ReleaseImage(app.ProjectName(), decision.Service, tag)
		if err := e.opts.Runner.Run(ctx, runner.Cmd{
			Name:   "docker",
			Args:   []string{"tag", decision.SourceImage, target},
			Stdout: log.Writer(),
			Stderr: log.Writer(),
		}); err != nil {
			log.Printf("%s: building (tagging %s failed: %v)", decision.Service, decision.SourceImage, err)
			rest = append(rest, decision.Service)
			continue
		}
		log.Printf("%s: reusing %s", decision.Service, decision.SourceImage)
	}
	return rest
}
