package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"aiops/internal/store"
)

type serverStore struct{ q querier }

const serverColumns = `id::text, name, device_kind, os, netdev_brand, access_proto, host, port,
	winrm_port, username, auth_method,
	coalesce(password_enc, ''), coalesce(private_key_enc, ''), coalesce(passphrase_enc, ''),
	coalesce(enable_secret_enc, ''), container_runtime,
	tags, auto_exec_enabled, host_key_fingerprint, host_key_public, host_key_verified_at,
	created_at, updated_at`

func scanServer(row pgx.Row) (store.Server, error) {
	var s store.Server
	err := row.Scan(
		&s.ID, &s.Name, &s.DeviceKind, &s.OS, &s.NetdevBrand, &s.AccessProto, &s.Host, &s.Port,
		&s.WinRMPort, &s.Username, &s.AuthMethod,
		&s.PasswordEncrypted, &s.PrivateKeyEncrypted, &s.PassphraseEncrypted,
		&s.EnableSecretEncrypted, &s.ContainerRuntime,
		&s.Tags, &s.AutoExecEnabled, &s.HostKeyFingerprint, &s.HostKeyPublic, &s.HostKeyVerifiedAt,
		&s.CreatedAt, &s.UpdatedAt,
	)
	if s.DeviceKind == "" {
		s.DeviceKind = store.DeviceKindServer
	}
	s.HasPassword = s.PasswordEncrypted != ""
	s.HasPrivateKey = s.PrivateKeyEncrypted != ""
	s.HasEnableSecret = s.EnableSecretEncrypted != ""
	if s.DeviceKind == store.DeviceKindServer {
		// server 类：brand 恒空、access_proto 恒 ssh（契约 2.6；防御旧数据）。
		s.NetdevBrand = ""
		s.AccessProto = store.AccessProtoSSH
	} else {
		// network_device 不探测容器运行时（契约 2.9）：恒空串。
		s.ContainerRuntime = store.ContainerRuntimeNone
	}
	if s.Tags == nil {
		s.Tags = []string{}
	}
	s.CreatedAt = utc(s.CreatedAt)
	s.UpdatedAt = utc(s.UpdatedAt)
	if s.HostKeyVerifiedAt != nil {
		t := utc(*s.HostKeyVerifiedAt)
		s.HostKeyVerifiedAt = &t
	}
	return s, mapError(err)
}

func (s serverStore) List(ctx context.Context) ([]store.Server, error) {
	rows, err := s.q.Query(ctx, `SELECT `+serverColumns+` FROM servers ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	items := make([]store.Server, 0)
	for rows.Next() {
		item, err := scanServer(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, mapError(rows.Err())
}

func (s serverStore) Get(ctx context.Context, id string) (store.Server, error) {
	return scanServer(s.q.QueryRow(ctx, `SELECT `+serverColumns+` FROM servers WHERE id = $1::uuid`, id))
}

func (s serverStore) Create(ctx context.Context, p store.CreateServerParams) (store.Server, error) {
	// os 缺省 linux、winrm_port 缺省 5985（契约 3.3；列有 NOT NULL DEFAULT，
	// 但显式传值避免依赖服务端缺省并让返回行与参数一致）。
	osVal := p.OS
	if osVal == "" {
		osVal = store.ServerOSLinux
	}
	// 三期（N1）：device_kind 缺省 server；network_device 时 os 恒 network、
	// access_proto 缺省 ssh（契约 2.6）。
	kind := p.DeviceKind
	if kind == "" {
		kind = store.DeviceKindServer
	}
	proto := p.AccessProto
	if proto == "" {
		proto = store.AccessProtoSSH
	}
	if kind == store.DeviceKindNetworkDevice {
		osVal = store.ServerOSNetwork
	} else {
		osVal = store.ServerOSLinux
		if p.OS == store.ServerOSWindows {
			osVal = store.ServerOSWindows
		}
	}
	winrmPort := p.WinRMPort
	if winrmPort == 0 {
		winrmPort = 5985
	}
	return scanServer(s.q.QueryRow(ctx, `
		INSERT INTO servers (name, device_kind, os, netdev_brand, access_proto, host, port,
			winrm_port, username, auth_method,
			password_enc, private_key_enc, passphrase_enc, enable_secret_enc,
			tags, auto_exec_enabled)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		RETURNING `+serverColumns,
		p.Name, string(kind), string(osVal), string(p.NetdevBrand), string(proto), p.Host, p.Port,
		winrmPort, p.Username, p.Credentials.AuthMethod,
		nullStr(p.Credentials.PasswordEncrypted),
		nullStr(p.Credentials.PrivateKeyEncrypted),
		nullStr(p.Credentials.PassphraseEncrypted),
		nullStr(p.EnableSecretEncrypted),
		nonNil(p.Tags), p.AutoExecEnabled))
}

func (s serverStore) Update(ctx context.Context, id string, p store.UpdateServerParams) (store.Server, error) {
	nameSet := p.Name != nil
	name := ""
	if nameSet {
		name = *p.Name
	}
	kindSet := p.DeviceKind != nil
	kind := ""
	if kindSet {
		kind = string(*p.DeviceKind)
	}
	osSet := p.OS != nil
	osVal := ""
	if osSet {
		osVal = string(*p.OS)
	}
	brandSet := p.NetdevBrand != nil
	brand := ""
	if brandSet {
		brand = string(*p.NetdevBrand)
	}
	protoSet := p.AccessProto != nil
	proto := ""
	if protoSet {
		proto = string(*p.AccessProto)
	}
	hostSet := p.Host != nil
	host := ""
	if hostSet {
		host = *p.Host
	}
	portSet := p.Port != nil
	port := 0
	if portSet {
		port = *p.Port
	}
	winrmPortSet := p.WinRMPort != nil
	winrmPort := 0
	if winrmPortSet {
		winrmPort = *p.WinRMPort
	}
	userSet := p.Username != nil
	username := ""
	if userSet {
		username = *p.Username
	}
	credSet := p.Credentials != nil
	authMethod := store.AuthMethod("")
	passwordEnc := any(nil)
	privateKeyEnc := any(nil)
	passphraseEnc := any(nil)
	if credSet {
		authMethod = p.Credentials.AuthMethod
		passwordEnc = nullStr(p.Credentials.PasswordEncrypted)
		privateKeyEnc = nullStr(p.Credentials.PrivateKeyEncrypted)
		passphraseEnc = nullStr(p.Credentials.PassphraseEncrypted)
	}
	// enable_secret：提供即替换（空串=清空 → NULL 列），nil=不变（契约 2.6）。
	enableSet := p.EnableSecretEncrypted != nil
	enableEnc := any(nil)
	if enableSet {
		enableEnc = nullStr(*p.EnableSecretEncrypted)
	}
	tagsSet := p.Tags != nil
	tags := []string{}
	if tagsSet {
		tags = nonNil(*p.Tags)
	}
	autoSet := p.AutoExecEnabled != nil
	auto := false
	if autoSet {
		auto = *p.AutoExecEnabled
	}

	return scanServer(s.q.QueryRow(ctx, `
		UPDATE servers SET
			name = CASE WHEN $1::boolean THEN $2::text ELSE name END,
			device_kind = CASE WHEN $3::boolean THEN $4::text ELSE device_kind END,
			os = CASE WHEN $5::boolean THEN $6::text ELSE os END,
			netdev_brand = CASE WHEN $7::boolean THEN $8::text ELSE netdev_brand END,
			access_proto = CASE WHEN $9::boolean THEN $10::text ELSE access_proto END,
			host = CASE WHEN $11::boolean THEN $12::text ELSE host END,
			port = CASE WHEN $13::boolean THEN $14::int ELSE port END,
			winrm_port = CASE WHEN $15::boolean THEN $16::int ELSE winrm_port END,
			username = CASE WHEN $17::boolean THEN $18::text ELSE username END,
			auth_method = CASE WHEN $19::boolean THEN $20::text ELSE auth_method END,
			password_enc = CASE WHEN $19::boolean THEN $21::text ELSE password_enc END,
			private_key_enc = CASE WHEN $19::boolean THEN $22::text ELSE private_key_enc END,
			passphrase_enc = CASE WHEN $19::boolean THEN $23::text ELSE passphrase_enc END,
			enable_secret_enc = CASE WHEN $24::boolean THEN $25::text ELSE enable_secret_enc END,
			tags = CASE WHEN $26::boolean THEN $27::text[] ELSE tags END,
			auto_exec_enabled = CASE WHEN $28::boolean THEN $29::boolean ELSE auto_exec_enabled END,
			host_key_fingerprint = CASE WHEN
				($11::boolean AND host IS DISTINCT FROM $12::text) OR
				($13::boolean AND port IS DISTINCT FROM $14::int)
				THEN '' ELSE host_key_fingerprint END,
			host_key_public = CASE WHEN
				($11::boolean AND host IS DISTINCT FROM $12::text) OR
				($13::boolean AND port IS DISTINCT FROM $14::int)
				THEN '' ELSE host_key_public END,
			host_key_verified_at = CASE WHEN
				($11::boolean AND host IS DISTINCT FROM $12::text) OR
				($13::boolean AND port IS DISTINCT FROM $14::int)
				THEN NULL ELSE host_key_verified_at END,
			updated_at = now()
		WHERE id = $30::uuid
		RETURNING `+serverColumns,
		nameSet, name, kindSet, kind, osSet, osVal, brandSet, brand, protoSet, proto,
		hostSet, host, portSet, port, winrmPortSet, winrmPort,
		userSet, username,
		credSet, string(authMethod), passwordEnc, privateKeyEnc, passphraseEnc,
		enableSet, enableEnc,
		tagsSet, tags, autoSet, auto, id))
}

func (s serverStore) Delete(ctx context.Context, id string) error {
	return deleteByID(ctx, s.q, "servers", id)
}

func (s serverStore) UpdateHostKey(ctx context.Context, id, fingerprint, publicKey string, at time.Time) (store.Server, error) {
	return scanServer(s.q.QueryRow(ctx, `
		UPDATE servers
		SET host_key_fingerprint = $2, host_key_public = $3,
		    host_key_verified_at = $4, updated_at = now()
		WHERE id = $1::uuid
		RETURNING `+serverColumns,
		id, fingerprint, publicKey, utc(at)))
}

// UpdateContainerRuntime 写回容器运行时探测缓存（四期 C1，契约 2.9 约定 1）。
// network_device 只允许空串（应用保证；违反返回 ErrInvalid）。
func (s serverStore) UpdateContainerRuntime(ctx context.Context, id string, runtime store.ContainerRuntime, at time.Time) (store.Server, error) {
	var kind store.DeviceKind
	if err := s.q.QueryRow(ctx, `SELECT device_kind FROM servers WHERE id = $1::uuid`, id).Scan(&kind); err != nil {
		return store.Server{}, mapError(err)
	}
	if kind == store.DeviceKindNetworkDevice && runtime != store.ContainerRuntimeNone {
		return store.Server{}, store.ErrInvalid
	}
	return scanServer(s.q.QueryRow(ctx, `
		UPDATE servers
		SET container_runtime = $2, updated_at = now()
		WHERE id = $1::uuid
		RETURNING `+serverColumns,
		id, string(runtime)))
}
