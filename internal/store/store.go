// Package store 是 Postgres 访问层（落地方案 §1：events/sessions/usage/outbox）。
//
// 提供：连接池 + 迁移执行。具体 repository 随 W1–W2 实现；
// events 与 outbox 必须同事务写入（落地方案 §5）。
package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store 持有连接池；worker 是事件唯一写入者，api 只读投影（订阅分发与计量聚合）。
type Store struct {
	Pool *pgxpool.Pool
}

// Open 建立连接池并 ping 验证。
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("store: create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &Store{Pool: pool}, nil
}

// Migrate 按文件名顺序执行 migrations/*.sql（建表均为 IF NOT EXISTS，幂等）。
// 说明：compose 环境已把本目录挂载进 postgres /docker-entrypoint-initdb.d，
// 本机裸跑 postgres 时用此方法初始化。
func (s *Store) Migrate(ctx context.Context) error {
	names, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("store: list migrations: %w", err)
	}
	sort.Strings(names)
	for _, name := range names {
		sqlBytes, err := migrationsFS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("store: read %s: %w", name, err)
		}
		if _, err := s.Pool.Exec(ctx, string(sqlBytes)); err != nil {
			return fmt.Errorf("store: apply %s: %w", name, err)
		}
	}
	return nil
}

// Close 释放连接池。
func (s *Store) Close() {
	s.Pool.Close()
}
