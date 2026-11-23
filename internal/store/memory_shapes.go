package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tricknotes/koroku/internal/compose"
)

// SetDeploymentMemoryShapes records the memory shape of each service of a
// deployment's release (ARCHITECTURE 6.2), so that the reservations the
// current app settings would give can be told while the release runs.
func (s *Store) SetDeploymentMemoryShapes(ctx context.Context, id int64, shapes map[string]compose.MemoryShape) error {
	if shapes == nil {
		shapes = map[string]compose.MemoryShape{}
	}
	data, err := json.Marshal(shapes)
	if err != nil {
		return fmt.Errorf("set memory shapes: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE deployments SET memory_shapes = ? WHERE id = ?`, string(data), id)
	if err != nil {
		return fmt.Errorf("set memory shapes: %w", err)
	}
	return requireOneRow(res, "set memory shapes")
}

// DeploymentMemoryShapes returns what SetDeploymentMemoryShapes recorded
// for each of the given deployments. Deployments without a record (made
// before shapes were recorded, or that do not exist) are left out.
func (s *Store) DeploymentMemoryShapes(ctx context.Context, ids ...int64) (map[int64]map[string]compose.MemoryShape, error) {
	out := map[int64]map[string]compose.MemoryShape{}
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(ids)), ", ")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, memory_shapes FROM deployments
		WHERE memory_shapes IS NOT NULL AND id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("memory shapes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var data string
		if err := rows.Scan(&id, &data); err != nil {
			return nil, fmt.Errorf("memory shapes: %w", err)
		}
		shapes := map[string]compose.MemoryShape{}
		if err := json.Unmarshal([]byte(data), &shapes); err != nil {
			return nil, fmt.Errorf("memory shapes of deployment %d: %w", id, err)
		}
		out[id] = shapes
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory shapes: %w", err)
	}
	return out, nil
}
