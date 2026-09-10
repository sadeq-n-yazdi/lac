package sqlite

import (
	"context"
	"time"

	"code.sadeq.uk/lac/internal/core"
)

type resourceRepository struct{ queries querier }

var _ core.ResourceRepository = resourceRepository{}

const resourceColumns = `name, capacity, lease_ttl_micros, description, created_at`

// Define creates the resource, or reconfigures it if it already exists. Reconfiguring never
// disturbs the leases already granted from it; a reduced capacity simply stops new grants until
// enough holders have released.
func (r resourceRepository) Define(ctx context.Context, resource core.Resource) error {
	if err := resource.Validate(); err != nil {
		return err
	}

	createdAt := resource.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	_, err := r.queries.ExecContext(ctx, `
		INSERT INTO resources (`+resourceColumns+`) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE
		   SET capacity         = excluded.capacity,
		       lease_ttl_micros = excluded.lease_ttl_micros,
		       description      = excluded.description`,
		resource.Name, resource.Capacity, resource.LeaseTimeToLive.Microseconds(),
		resource.Description, requireMicros(createdAt),
	)

	return translateError("defining resource "+resource.Name, err)
}

func (r resourceRepository) ByName(ctx context.Context, name string) (core.Resource, error) {
	row := r.queries.QueryRowContext(ctx, `SELECT `+resourceColumns+` FROM resources WHERE name = ?`, name)

	resource, err := scanResource(row)
	if err != nil {
		return core.Resource{}, translateError("reading resource "+name, err)
	}

	return resource, nil
}

func (r resourceRepository) List(ctx context.Context) ([]core.Resource, error) {
	rows, err := r.queries.QueryContext(ctx, `SELECT `+resourceColumns+` FROM resources ORDER BY name`)
	if err != nil {
		return nil, translateError("listing resources", err)
	}
	defer func() { _ = rows.Close() }()

	resources := make([]core.Resource, 0, 8)
	for rows.Next() {
		resource, err := scanResource(rows)
		if err != nil {
			return nil, translateError("listing resources", err)
		}
		resources = append(resources, resource)
	}
	if err := rows.Err(); err != nil {
		return nil, translateError("listing resources", err)
	}

	return resources, nil
}

func (r resourceRepository) Delete(ctx context.Context, name string) error {
	result, err := r.queries.ExecContext(ctx, `DELETE FROM resources WHERE name = ?`, name)

	return affectedOrNotFound("deleting resource "+name, result, err)
}

func scanResource(source scanner) (core.Resource, error) {
	var (
		resource      core.Resource
		timeToLive    int64
		createdAtMicr int64
	)

	if err := source.Scan(&resource.Name, &resource.Capacity, &timeToLive,
		&resource.Description, &createdAtMicr); err != nil {
		return core.Resource{}, err
	}

	resource.LeaseTimeToLive = time.Duration(timeToLive) * time.Microsecond
	resource.CreatedAt = fromRequiredMicros(createdAtMicr)

	return resource, nil
}
