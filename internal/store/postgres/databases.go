package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"aiops/internal/store"
)

type databaseStore struct{ q querier }

const databaseColumns = `id::text, name, engine, host, port, instance_name, database_name, username,
	coalesce(password_enc, ''), redis_db, auth_db, sqlite_path, ssl_mode, created_at, updated_at`

func scanDatabase(row pgx.Row) (store.Database, error) {
	var d store.Database
	err := row.Scan(
		&d.ID, &d.Name, &d.Engine, &d.Host, &d.Port, &d.InstanceName, &d.DatabaseName, &d.Username,
		&d.PasswordEncrypted, &d.RedisDB, &d.AuthDB, &d.SQLitePath, &d.SSLMode, &d.CreatedAt, &d.UpdatedAt,
	)
	d.HasPassword = d.PasswordEncrypted != ""
	d.CreatedAt = utc(d.CreatedAt)
	d.UpdatedAt = utc(d.UpdatedAt)
	return d, mapError(err)
}

func (s databaseStore) List(ctx context.Context) ([]store.Database, error) {
	rows, err := s.q.Query(ctx, `SELECT `+databaseColumns+` FROM databases ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	items := make([]store.Database, 0)
	for rows.Next() {
		item, err := scanDatabase(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, mapError(rows.Err())
}

func (s databaseStore) Get(ctx context.Context, id string) (store.Database, error) {
	return scanDatabase(s.q.QueryRow(ctx, `SELECT `+databaseColumns+` FROM databases WHERE id = $1::uuid`, id))
}

func (s databaseStore) Create(ctx context.Context, p store.CreateDatabaseParams) (store.Database, error) {
	return scanDatabase(s.q.QueryRow(ctx, `
		INSERT INTO databases (name, engine, host, port, instance_name, database_name, username,
			password_enc, redis_db, auth_db, sqlite_path, ssl_mode)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING `+databaseColumns,
		p.Name, p.Engine, p.Host, p.Port, p.InstanceName, p.DatabaseName, p.Username,
		nullStr(p.Credentials.PasswordEncrypted), p.RedisDB, p.AuthDB, p.SQLitePath, p.SSLMode))
}

func (s databaseStore) Update(ctx context.Context, id string, p store.UpdateDatabaseParams) (store.Database, error) {
	nameSet, name := p.Name != nil, ""
	if nameSet {
		name = *p.Name
	}
	hostSet, host := p.Host != nil, ""
	if hostSet {
		host = *p.Host
	}
	portSet, port := p.Port != nil, 0
	if portSet {
		port = *p.Port
	}
	dbSet, dbName := p.DatabaseName != nil, ""
	if dbSet {
		dbName = *p.DatabaseName
	}
	userSet, username := p.Username != nil, ""
	if userSet {
		username = *p.Username
	}
	instanceSet, instance := p.InstanceName != nil, ""
	if instanceSet {
		instance = *p.InstanceName
	}
	credSet := p.Credentials != nil
	passwordEnc := any(nil)
	if credSet {
		passwordEnc = nullStr(p.Credentials.PasswordEncrypted)
	}
	redisDBSet, redisDB := p.RedisDB != nil, 0
	if redisDBSet {
		redisDB = *p.RedisDB
	}
	authDBSet, authDB := p.AuthDB != nil, ""
	if authDBSet {
		authDB = *p.AuthDB
	}
	pathSet, sqlitePath := p.SQLitePath != nil, ""
	if pathSet {
		sqlitePath = *p.SQLitePath
	}
	sslSet, sslMode := p.SSLMode != nil, store.SSLMode("")
	if sslSet {
		sslMode = *p.SSLMode
	}

	return scanDatabase(s.q.QueryRow(ctx, `
		UPDATE databases SET
			name = CASE WHEN $1::boolean THEN $2::text ELSE name END,
			host = CASE WHEN $3::boolean THEN $4::text ELSE host END,
			port = CASE WHEN $5::boolean THEN $6::int ELSE port END,
			instance_name = CASE WHEN $7::boolean THEN $8::text ELSE instance_name END,
			database_name = CASE WHEN $9::boolean THEN $10::text ELSE database_name END,
			username = CASE WHEN $11::boolean THEN $12::text ELSE username END,
			password_enc = CASE WHEN $13::boolean THEN $14::text ELSE password_enc END,
			redis_db = CASE WHEN $15::boolean THEN $16::int ELSE redis_db END,
			auth_db = CASE WHEN $17::boolean THEN $18::text ELSE auth_db END,
			sqlite_path = CASE WHEN $19::boolean THEN $20::text ELSE sqlite_path END,
			ssl_mode = CASE WHEN $21::boolean THEN $22::text ELSE ssl_mode END,
			updated_at = now()
		WHERE id = $23::uuid
		RETURNING `+databaseColumns,
		nameSet, name, hostSet, host, portSet, port, instanceSet, instance, dbSet, dbName,
		userSet, username, credSet, passwordEnc, redisDBSet, redisDB, authDBSet, authDB,
		pathSet, sqlitePath, sslSet, string(sslMode), id))
}

func (s databaseStore) Delete(ctx context.Context, id string) error {
	return deleteByID(ctx, s.q, "databases", id)
}
