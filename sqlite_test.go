package main

import (
	"strings"
	"testing"
)

// TestSqlitePathUsesAmpersand 锁住踩过的坑：参数分隔符是 "&"，
// 写成 "(" 会被 mattn/go-sqlite3 静默忽略，高并发下随机 "database is locked"。
func TestSqlitePathUsesAmpersand(t *testing.T) {
	got := sqlitePath("/data/locks.db")
	for _, want := range []string{"journal_mode=WAL", "busy_timeout=5000",
		"synchronous=FULL", "txlock=immediate"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %s: %s", want, got)
		}
	}
	if strings.Contains(got, "(") {
		t.Errorf("不能含 '(': %s", got)
	}
	if !strings.Contains(got, "wal_autocheckpoint=1000") {
		t.Errorf("缺少 wal_autocheckpoint，WAL 会无限膨胀: %s", got)
	}
}

// TestSqlitePathAppendsToExistingQuery 验证已有查询串时也要追加参数。
func TestSqlitePathAppendsToExistingQuery(t *testing.T) {
	got := sqlitePath("/data/locks.db?cache=shared")
	if !strings.Contains(got, "cache=shared&") {
		t.Errorf("已有参数后应追加: %s", got)
	}
	if !strings.Contains(got, "busy_timeout=5000") {
		t.Errorf("追加后仍应包含并发参数: %s", got)
	}
}
