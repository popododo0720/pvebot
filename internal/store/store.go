// Package store keeps accounts, bot records, and the message bus.
// Account secrets are encrypted with AES-GCM before they touch the database.
package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

const (
	StatusPending      = "pending"
	StatusRunning      = "running"
	StatusDone         = "done"
	StatusError        = "error"
	StatusNeedsRelogin = "needs_relogin"

	AuthOK           = "ok"
	AuthNeedsRelogin = "needs_relogin"
)

// ErrNotFound means the row is absent.
var ErrNotFound = errors.New("not found")

// ErrExists means the name is already used.
var ErrExists = errors.New("already exists")

// Account is a stored credential without its secret.
type Account struct {
	Name string
	Kind string
}

// Bot records a bot. The token itself is not stored, only its hash.
type Bot struct {
	Name      string
	Backend   string
	Node      string
	Account   string
	AuthState string
}

type Message struct {
	ID     int64  `json:"id"`
	Bot    string `json:"bot"`
	Sender string `json:"sender"`
	Body   string `json:"body"`
	Reply  string `json:"reply"`
	Status string `json:"status"`
}

// Store is safe for concurrent use.
type Store struct {
	db   *sql.DB
	key  []byte
	mu   sync.Mutex
	cond *sync.Cond
}

// Open opens the database. key must be 32 bytes.
func Open(path string, key []byte) (*Store, error) {
	if len(key) != 32 {
		return nil, errors.New("master key must be 32 bytes")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, key: append([]byte(nil), key...)}
	s.cond = sync.NewCond(&s.mu)
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS accounts (
  name TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  nonce BLOB NOT NULL,
  ciphertext BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS bots (
  name TEXT PRIMARY KEY,
  backend TEXT NOT NULL,
  node TEXT NOT NULL,
  account TEXT NOT NULL,
  token_hash TEXT NOT NULL UNIQUE
);
CREATE TABLE IF NOT EXISTS messages (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  bot TEXT NOT NULL,
  sender TEXT NOT NULL,
  body TEXT NOT NULL,
  reply TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL
);
`)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`ALTER TABLE bots ADD COLUMN auth_state TEXT NOT NULL DEFAULT 'ok'`)
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		return err
	}
	return nil
}

// PutAccount encrypts secret and inserts the account.
func (s *Store) PutAccount(name, kind, secret string) error {
	nonce, ct, err := encrypt(s.key, []byte(secret))
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.db.Exec(`INSERT INTO accounts(name, kind, nonce, ciphertext) VALUES(?,?,?,?)`, name, kind, nonce, ct)
	if err != nil && isConstraint(err) {
		return ErrExists
	}
	return err
}

// Accounts lists names and kinds. Secrets stay in the database.
func (s *Store) Accounts() ([]Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT name, kind FROM accounts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.Name, &a.Kind); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) Secret(name string) (kind string, secret Plaintext, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var nonce, ct []byte
	err = s.db.QueryRow(`SELECT kind, nonce, ciphertext FROM accounts WHERE name=?`, name).Scan(&kind, &nonce, &ct)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, ErrNotFound
	}
	if err != nil {
		return "", nil, err
	}
	plain, err := decrypt(s.key, nonce, ct)
	if err != nil {
		return "", nil, err
	}
	return kind, Plaintext(plain), nil
}

// InsertBot records a bot and the hash of its daemon token.
func (s *Store) InsertBot(b Bot, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO bots(name, backend, node, account, token_hash) VALUES(?,?,?,?,?)`,
		b.Name, b.Backend, b.Node, b.Account, tokenHash(token))
	if err != nil && isConstraint(err) {
		return ErrExists
	}
	return err
}

// DeleteBot removes the bot and its messages.
func (s *Store) DeleteBot(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`DELETE FROM messages WHERE bot=?`, name); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM bots WHERE name=?`, name)
	return err
}

func (s *Store) Bots() ([]Bot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT name, backend, node, account, auth_state FROM bots ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Bot
	for rows.Next() {
		var b Bot
		if err := rows.Scan(&b.Name, &b.Backend, &b.Node, &b.Account, &b.AuthState); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) BotByToken(token string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var name string
	err := s.db.QueryRow(`SELECT name FROM bots WHERE token_hash=?`, tokenHash(token)).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return name, err
}

func (s *Store) GetBot(name string) (Bot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b Bot
	err := s.db.QueryRow(`SELECT name, backend, node, account, auth_state FROM bots WHERE name=?`, name).Scan(&b.Name, &b.Backend, &b.Node, &b.Account, &b.AuthState)
	if errors.Is(err, sql.ErrNoRows) {
		return Bot{}, ErrNotFound
	}
	return b, err
}

func (s *Store) Enqueue(bot, sender, body string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`INSERT INTO messages(bot, sender, body, status) VALUES(?,?,?,?)`, bot, sender, body, StatusPending)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	s.cond.Broadcast()
	return id, nil
}

// Claim takes the oldest pending message for a bot.
func (s *Store) Claim(bot string) (Message, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var m Message
	err := s.db.QueryRow(`SELECT id, bot, sender, body, reply, status FROM messages WHERE bot=? AND status=? ORDER BY id LIMIT 1`, bot, StatusPending).
		Scan(&m.ID, &m.Bot, &m.Sender, &m.Body, &m.Reply, &m.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, false, nil
	}
	if err != nil {
		return Message{}, false, err
	}
	res, err := s.db.Exec(`UPDATE messages SET status=? WHERE id=? AND status=?`, StatusRunning, m.ID, StatusPending)
	if err != nil {
		return Message{}, false, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return Message{}, false, nil
	}
	m.Status = StatusRunning
	return m, true, nil
}

func (s *Store) Complete(bot string, id int64, reply, status string) error {
	switch status {
	case StatusDone, StatusError, StatusNeedsRelogin:
	default:
		return fmt.Errorf("unknown message status %q", status)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE messages SET reply=?, status=? WHERE id=? AND bot=? AND status=?`, reply, status, id, bot, StatusRunning)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrNotFound
	}
	if status == StatusDone || status == StatusNeedsRelogin {
		auth := AuthOK
		if status == StatusNeedsRelogin {
			auth = AuthNeedsRelogin
		}
		if _, err := s.db.Exec(`UPDATE bots SET auth_state=? WHERE name=?`, auth, bot); err != nil {
			return err
		}
	}
	s.cond.Broadcast()
	return nil
}

func (s *Store) AppendReply(bot string, id int64, chunk string) error {
	if chunk == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	err := s.db.QueryRow(`SELECT length(reply) FROM messages WHERE id=? AND bot=? AND status=?`, id, bot, StatusRunning).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	const maxReply = 1 << 20
	if n >= maxReply {
		return nil
	}
	if n+len(chunk) > maxReply {
		chunk = chunk[:maxReply-n]
	}
	_, err = s.db.Exec(`UPDATE messages SET reply = reply || ? WHERE id=? AND bot=? AND status=?`, chunk, id, bot, StatusRunning)
	return err
}

func (s *Store) Get(id int64) (Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(id)
}

func (s *Store) get(id int64) (Message, error) {
	var m Message
	err := s.db.QueryRow(`SELECT id, bot, sender, body, reply, status FROM messages WHERE id=?`, id).
		Scan(&m.ID, &m.Bot, &m.Sender, &m.Body, &m.Reply, &m.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	return m, err
}

// Messages lists the transcript for a bot, oldest first.
func (s *Store) Messages(bot string) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT id, bot, sender, body, reply, status FROM messages WHERE bot=? ORDER BY id`, bot)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.Bot, &m.Sender, &m.Body, &m.Reply, &m.Status); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Wait blocks until the message is done or the context ends.
func (s *Store) Wait(ctx context.Context, id int64) (Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stop := context.AfterFunc(ctx, func() {
		s.mu.Lock()
		s.cond.Broadcast()
		s.mu.Unlock()
	})
	defer stop()
	for {
		m, err := s.get(id)
		if err != nil {
			return Message{}, err
		}
		if m.Status == StatusDone || m.Status == StatusError || m.Status == StatusNeedsRelogin {
			return m, nil
		}
		if ctx.Err() != nil {
			return Message{}, ctx.Err()
		}
		s.cond.Wait()
	}
}

type Plaintext []byte

func (p Plaintext) String() string   { return "[redacted]" }
func (p Plaintext) GoString() string { return "[redacted]" }
func (p Plaintext) Reveal() string   { return string(p) }

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func encrypt(key, plain []byte) (nonce, ciphertext []byte, err error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, err
	}
	return nonce, gcm.Seal(nil, nonce, plain, nil), nil
}

func decrypt(key, nonce, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt account: %w", err)
	}
	return plain, nil
}

func isConstraint(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "constraint")
}
