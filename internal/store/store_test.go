package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func testKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}

func TestAccountRoundTripHidesSecret(t *testing.T) {
	s := openTest(t)
	const secret = "sk-ant-test-token"
	if err := s.PutAccount("claude-main", "claude", secret); err != nil {
		t.Fatal(err)
	}
	if err := s.PutAccount("claude-main", "claude", secret); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate account err = %v", err)
	}
	accounts, err := s.Accounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || accounts[0].Name != "claude-main" || accounts[0].Kind != "claude" {
		t.Fatalf("accounts = %+v", accounts)
	}
	raw, err := osReadDB(s)
	if err != nil {
		t.Fatal(err)
	}
	if contains(raw, secret) {
		t.Fatal("database file contains the plaintext secret")
	}
	kind, got, err := s.Secret("claude-main")
	if err != nil {
		t.Fatal(err)
	}
	if kind != "claude" || got.Reveal() != secret {
		t.Fatalf("secret = %s %q", kind, got.Reveal())
	}
	if fmt.Sprint(got) != "[redacted]" || fmt.Sprintf("%#v", got) != "[redacted]" {
		t.Fatalf("printed secret = %s %#v", got, got)
	}
}

func TestWrongKeyCannotRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pvebot.db")
	s, err := Open(path, testKey())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutAccount("a", "apikey", "secret-value"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	other := testKey()
	other[0] = 9
	s2, err := Open(path, other)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	_, got, err := s2.Secret("a")
	if err == nil || got.Reveal() != "" {
		t.Fatal("expected decrypt failure")
	}
}

func TestBusClaimAndWait(t *testing.T) {
	s := openTest(t)
	if err := s.InsertBot(Bot{Name: "a", Backend: "stub", Node: "n1"}, "token-a"); err != nil {
		t.Fatal(err)
	}
	id, err := s.Enqueue("a", "user", "hello")
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Claim("a")
	if err != nil || !ok || got.Body != "hello" || got.ID != id {
		t.Fatalf("claim = %+v ok=%v err=%v", got, ok, err)
	}
	_, ok, err = s.Claim("a")
	if err != nil || ok {
		t.Fatalf("second claim ok=%v err=%v", ok, err)
	}
	done := make(chan Message, 1)
	go func() {
		msg, err := s.Wait(context.Background(), id)
		if err != nil {
			t.Error(err)
			return
		}
		done <- msg
	}()
	time.Sleep(50 * time.Millisecond)
	if err := s.AppendReply("a", id, "ack:"); err != nil {
		t.Fatal(err)
	}
	partial, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if partial.Status != StatusRunning || partial.Reply != "ack:" {
		t.Fatalf("partial = %+v", partial)
	}
	if err := s.Complete("a", id, "ack:hello", StatusDone); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-done:
		if msg.Status != StatusDone || msg.Reply != "ack:hello" {
			t.Fatalf("wait = %+v", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not return")
	}
	name, err := s.BotByToken("token-a")
	if err != nil || name != "a" {
		t.Fatalf("token lookup = %s %v", name, err)
	}
	if _, err := s.BotByToken("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad token err = %v", err)
	}
	bot, err := s.GetBot("a")
	if err != nil || bot.AuthState != AuthOK {
		t.Fatalf("auth state after success = %+v %v", bot, err)
	}
}

func TestNeedsReloginUnblocksWait(t *testing.T) {
	s := openTest(t)
	if err := s.InsertBot(Bot{Name: "a", Backend: "claude", Node: "n1"}, "token-a"); err != nil {
		t.Fatal(err)
	}
	id, err := s.Enqueue("a", "user", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Claim("a"); err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	done := make(chan Message, 1)
	go func() {
		msg, err := s.Wait(context.Background(), id)
		if err != nil {
			t.Error(err)
			return
		}
		done <- msg
	}()
	time.Sleep(30 * time.Millisecond)
	if err := s.Complete("a", id, "needs re-login: token expired", StatusNeedsRelogin); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-done:
		if msg.Status != StatusNeedsRelogin {
			t.Fatalf("wait = %+v", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not return")
	}
	bot, err := s.GetBot("a")
	if err != nil || bot.AuthState != AuthNeedsRelogin {
		t.Fatalf("auth state = %+v %v", bot, err)
	}
}

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "pvebot.db"), testKey())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func osReadDB(s *Store) (string, error) {
	rows, err := s.db.Query(`SELECT ciphertext FROM accounts`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var all string
	for rows.Next() {
		var ct []byte
		if err := rows.Scan(&ct); err != nil {
			return "", err
		}
		all += string(ct)
	}
	return all, rows.Err()
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (s == sub || len(s) > 0 && index(s, sub))
}

func index(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
