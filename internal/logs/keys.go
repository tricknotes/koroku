package logs

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/objstore"
)

// archivePrefix is where all archived logs live in the bucket. The bucket's
// lifecycle rule is attached to it.
const archivePrefix = "logs/"

// ErrInvalidArchiveKey is returned when an object key is not an archived log
// of the app it is claimed for.
var ErrInvalidArchiveKey = errors.New("logs: not an archive key of this app")

var (
	// <service>/YYYY/MM/DD/HH.log.gz
	windowKeyPattern = regexp.MustCompile(`^([^/]+)/([0-9]{4})/([0-9]{2})/([0-9]{2})/([0-9]{2})\.log\.gz$`)
	// _deploy/<id>.log.gz
	deployKeyPattern = regexp.MustCompile(`^` + DeployService + `/([1-9][0-9]{0,17})\.log\.gz$`)
)

// Archive is one object of archived logs.
type Archive struct {
	Key     string
	Service string
	// Hour is the hour the object's window started in, in the location
	// passed to ListArchives. Deploy logs have no window; theirs is the
	// hour they were uploaded.
	Hour time.Time
	Size int64
	// DeployID is the deployment a DeployService object belongs to, and
	// zero for everything else.
	DeployID int64
}

func appPrefix(slug string) string { return archivePrefix + slug + "/" }

// windowKey is the object key of the window of service that starts at start.
func windowKey(slug, service string, start time.Time, loc *time.Location) string {
	t := start.In(loc)
	return fmt.Sprintf("%s%s/%04d/%02d/%02d/%02d.log.gz", appPrefix(slug), service, t.Year(), int(t.Month()), t.Day(), t.Hour())
}

func deployKey(slug string, id int64) string {
	return fmt.Sprintf("%s%s/%d.log.gz", appPrefix(slug), DeployService, id)
}

// parsed is what an archive key says about its object.
type parsedKey struct {
	service  string
	deployID int64
	// year to hour are only set for window keys.
	year, month, day, hour int
}

func parseArchiveKey(slug, key string) (parsedKey, bool) {
	rest, ok := strings.CutPrefix(key, appPrefix(slug))
	if !ok {
		return parsedKey{}, false
	}
	if m := deployKeyPattern.FindStringSubmatch(rest); m != nil {
		id, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return parsedKey{}, false
		}
		return parsedKey{service: DeployService, deployID: id}, true
	}
	m := windowKeyPattern.FindStringSubmatch(rest)
	if m == nil || m[1] == DeployService || checkService(m[1]) != nil {
		return parsedKey{}, false
	}
	p := parsedKey{service: m[1]}
	p.year, _ = strconv.Atoi(m[2])
	p.month, _ = strconv.Atoi(m[3])
	p.day, _ = strconv.Atoi(m[4])
	p.hour, _ = strconv.Atoi(m[5])
	// Reject dates that only exist after normalization (month 13, hour 24).
	t := time.Date(p.year, time.Month(p.month), p.day, p.hour, 0, 0, 0, time.UTC)
	if t.Year() != p.year || int(t.Month()) != p.month || t.Day() != p.day || t.Hour() != p.hour {
		return parsedKey{}, false
	}
	return p, true
}

// ValidateArchiveKey reports whether key names an archived log of the app
// slug, as written by the Archiver. It must pass before a download URL is
// issued for a key that came from a client: keys of other apps, keys outside
// the log prefix and anything that is not exactly of the archived form are
// rejected with ErrInvalidArchiveKey.
func ValidateArchiveKey(slug, key string) error {
	if err := checkSlug(slug); err != nil {
		return err
	}
	if err := objstore.ValidateKey(key); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidArchiveKey, err)
	}
	if _, ok := parseArchiveKey(slug, key); !ok {
		return ErrInvalidArchiveKey
	}
	return nil
}

// ListArchives returns the archived logs of slug, newest first. An empty
// service lists all services, including AccessService and DeployService.
// Objects below the app's prefix that are not archives are left out.
func ListArchives(ctx context.Context, store objstore.Store, loc *time.Location, slug, service string) ([]Archive, error) {
	if err := checkSlug(slug); err != nil {
		return nil, err
	}
	prefix := appPrefix(slug)
	if service != "" {
		if err := checkService(service); err != nil {
			return nil, err
		}
		prefix += service + "/"
	}
	if loc == nil {
		loc = time.UTC
	}
	objects, err := store.List(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("list archived logs: %w", err)
	}
	archives := make([]Archive, 0, len(objects))
	for _, object := range objects {
		p, ok := parseArchiveKey(slug, object.Key)
		if !ok {
			continue
		}
		a := Archive{Key: object.Key, Service: p.service, Size: object.Size, DeployID: p.deployID}
		if p.service == DeployService {
			m := object.LastModified.In(loc)
			a.Hour = time.Date(m.Year(), m.Month(), m.Day(), m.Hour(), 0, 0, 0, loc)
		} else {
			a.Hour = time.Date(p.year, time.Month(p.month), p.day, p.hour, 0, 0, 0, loc)
		}
		archives = append(archives, a)
	}
	sort.SliceStable(archives, func(i, j int) bool {
		a, b := archives[i], archives[j]
		if !a.Hour.Equal(b.Hour) {
			return a.Hour.After(b.Hour)
		}
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		return a.Key > b.Key
	})
	return archives, nil
}
