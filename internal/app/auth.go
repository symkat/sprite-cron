package app

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}
func randomID() string       { return hex.EncodeToString(randomBytes(12)) }
func digest(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func PasswordHash(password string) (string, error) {
	if len(password) < 12 || len(password) > 1024 {
		return "", errors.New("password must be 12–1024 bytes")
	}
	salt := randomBytes(16)
	hash := argon2.IDKey([]byte(password), salt, 2, 19*1024, 1, 32)
	return "argon2id:2:19456:1:" + base64.RawStdEncoding.EncodeToString(salt) + ":" + base64.RawStdEncoding.EncodeToString(hash), nil
}
func PasswordOK(encoded, password string) bool {
	parts := strings.Split(encoded, ":")
	if len(parts) != 6 || strings.Join(parts[:4], ":") != "argon2id:2:19456:1" {
		return false
	}
	salt, e := base64.RawStdEncoding.DecodeString(parts[4])
	if e != nil || len(salt) != 16 {
		return false
	}
	hash, e := base64.RawStdEncoding.DecodeString(parts[5])
	if e != nil || len(hash) != 32 || len(password) > 1024 {
		return false
	}
	test := argon2.IDKey([]byte(password), salt, 2, 19*1024, 1, 32)
	return subtle.ConstantTimeCompare(test, hash) == 1
}

type Vault struct {
	Keys   map[string][]byte
	Active string
}

func NewVault(spec, active string) (*Vault, error) {
	var keys map[string]string
	if err := json.Unmarshal([]byte(spec), &keys); err != nil {
		return nil, errors.New("SPRITE_CRON_KEYS must be JSON mapping key IDs to base64-encoded 32-byte keys")
	}
	v := &Vault{Keys: map[string][]byte{}, Active: active}
	for id, b64 := range keys {
		b, e := base64.StdEncoding.DecodeString(b64)
		if e != nil || len(b) != 32 || !identifier.MatchString(id) {
			return nil, errors.New("invalid encryption key")
		}
		v.Keys[id] = b
	}
	if len(v.Keys[active]) != 32 {
		return nil, errors.New("SPRITE_CRON_ACTIVE_KEY does not exist in SPRITE_CRON_KEYS")
	}
	return v, nil
}
func (v *Vault) Seal(id, value string) (string, error) {
	block, e := aes.NewCipher(v.Keys[v.Active])
	if e != nil {
		return "", e
	}
	g, e := cipher.NewGCM(block)
	if e != nil {
		return "", e
	}
	nonce := randomBytes(g.NonceSize())
	b := g.Seal(nonce, nonce, []byte(value), []byte(id))
	return v.Active + ":" + base64.RawStdEncoding.EncodeToString(b), nil
}
func (v *Vault) Open(id, value string) (string, error) {
	parts := strings.SplitN(value, ":", 2)
	if len(parts) != 2 {
		return "", errors.New("invalid encrypted credential")
	}
	key := v.Keys[parts[0]]
	if len(key) != 32 {
		return "", errors.New("credential encryption key unavailable")
	}
	b, e := base64.RawStdEncoding.DecodeString(parts[1])
	if e != nil {
		return "", e
	}
	block, _ := aes.NewCipher(key)
	g, _ := cipher.NewGCM(block)
	if len(b) < g.NonceSize() {
		return "", errors.New("invalid encrypted credential")
	}
	plain, e := g.Open(nil, b[:g.NonceSize()], b[g.NonceSize():], []byte(id))
	if e != nil {
		return "", errors.New("credential decryption failed")
	}
	return string(plain), nil
}
func (s *Store) Secret(v *Vault, id string) (string, error) {
	var value string
	err := s.DB.QueryRow(`SELECT encrypted FROM credentials WHERE id=?`, id).Scan(&value)
	if err != nil {
		return "", errors.New("credential unavailable")
	}
	return v.Open(id, value)
}

type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	Disabled bool   `json:"disabled"`
	Service  bool   `json:"service_account"`
	Password string `json:"-"`
}

func validRole(r string) bool { return r == "admin" || r == "operator" || r == "reader" }
func userFrom(q querier, column, value string) (User, error) {
	var u User
	if column != "id" && column != "username" {
		return u, errors.New("invalid lookup")
	}
	err := q.QueryRow(`SELECT id,username,role,disabled,service,password FROM users WHERE `+column+`=?`, value).Scan(&u.ID, &u.Username, &u.Role, &u.Disabled, &u.Service, &u.Password)
	return u, err
}
func (s *Store) AddUser(name, role, password string, service bool) error {
	if !identifier.MatchString(name) || !validRole(role) {
		return errors.New("invalid username or role")
	}
	hash := ""
	var err error
	if !service {
		hash, err = PasswordHash(password)
		if err != nil {
			return err
		}
	}
	return transaction(s.DB, func(tx *sql.Tx) error {
		_, e := tx.Exec(`INSERT INTO users(id,username,role,password,service) VALUES(?,?,?,?,?)`, randomID(), name, role, hash, service)
		if e != nil {
			return e
		}
		return audit(tx, "local-cli", "user.create", name, role)
	})
}
func (s *Store) ChangeUser(name, action, value, actor string) error {
	return transaction(s.DB, func(tx *sql.Tx) error {
		u, e := userFrom(tx, "username", name)
		if e != nil {
			return e
		}
		switch action {
		case "password":
			if u.Service {
				return errors.New("service accounts cannot have passwords")
			}
			h, e := PasswordHash(value)
			if e != nil {
				return e
			}
			_, e = tx.Exec(`UPDATE users SET password=? WHERE id=?`, h, u.ID)
			if e != nil {
				return e
			}
		case "disable":
			_, e = tx.Exec(`UPDATE users SET disabled=1 WHERE id=?`, u.ID)
		case "enable":
			_, e = tx.Exec(`UPDATE users SET disabled=0 WHERE id=?`, u.ID)
		case "role":
			if !validRole(value) {
				return errors.New("invalid role")
			}
			_, e = tx.Exec(`UPDATE users SET role=? WHERE id=?`, value, u.ID)
		default:
			return errors.New("unknown user action")
		}
		if e != nil {
			return e
		}
		if action == "password" || action == "disable" {
			if _, e = tx.Exec(`DELETE FROM sessions WHERE user_id=?`, u.ID); e != nil {
				return e
			}
			if _, e = tx.Exec(`UPDATE api_tokens SET revoked=1 WHERE user_id=?`, u.ID); e != nil {
				return e
			}
		}
		return audit(tx, actor, "user."+action, name, "")
	})
}
func (s *Store) Users() ([]User, error) {
	rows, e := s.DB.Query(`SELECT id,username,role,disabled,service FROM users ORDER BY username`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	list := []User{}
	for rows.Next() {
		var u User
		if e = rows.Scan(&u.ID, &u.Username, &u.Role, &u.Disabled, &u.Service); e != nil {
			return nil, e
		}
		list = append(list, u)
	}
	return list, rows.Err()
}

var Scopes = []string{"jobs:read", "jobs:write", "runs:read", "runs:trigger", "runs:cancel", "targets:read", "targets:write"}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

type TokenInfo struct {
	ID       string   `json:"id"`
	UserID   string   `json:"user_id"`
	Name     string   `json:"name"`
	Scopes   []string `json:"scopes"`
	Targets  []string `json:"targets"`
	Expires  int64    `json:"expires_at"`
	Revoked  bool     `json:"revoked"`
	LastUsed int64    `json:"last_used_at"`
}

func (s *Store) NewToken(username, name string, scopes, targets []string, ttl time.Duration, actor string) (string, TokenInfo, error) {
	var info TokenInfo
	u, e := userFrom(s.DB, "username", username)
	if e != nil {
		return "", info, e
	}
	if u.Disabled {
		return "", info, ErrForbidden
	}
	if name == "" || len(name) > 120 || ttl < time.Minute || ttl > 366*24*time.Hour || len(scopes) == 0 || len(targets) == 0 {
		return "", info, errors.New("name, scopes, targets and expiry (1 minute–366 days) required")
	}
	for _, scope := range scopes {
		if scope == "targets:write" && u.Role != "admin" {
			return "", info, ErrForbidden
		}
		if !contains(Scopes, scope) || u.Role == "reader" && !strings.HasSuffix(scope, ":read") {
			return "", info, ErrForbidden
		}
	}
	for _, target := range targets {
		if target == "*" {
			if u.Role != "admin" {
				return "", info, errors.New("all-target tokens require an administrator")
			}
			continue
		}
		if !identifier.MatchString(target) {
			return "", info, errors.New("invalid target ID")
		}
		// Provisioning tokens may name a target before it exists.
		if contains(scopes, "targets:write") {
			continue
		}
		if _, e = targetFrom(s.DB, target); e != nil {
			return "", info, errors.New("unknown target")
		}
	}
	info = TokenInfo{ID: randomID(), UserID: u.ID, Name: name, Scopes: scopes, Targets: targets, Expires: time.Now().Add(ttl).UnixMilli()}
	secret := "scron_" + info.ID + "_" + hex.EncodeToString(randomBytes(32))
	e = transaction(s.DB, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO api_tokens(id,digest,user_id,name,scopes,targets,expires) VALUES(?,?,?,?,?,?,?)`, info.ID, digest(secret), u.ID, name, encoded(scopes), encoded(targets), info.Expires)
		if err != nil {
			return err
		}
		return audit(tx, actor, "token.create", info.ID, "")
	})
	return secret, info, e
}
func (s *Store) Tokens() ([]TokenInfo, error) {
	rows, e := s.DB.Query(`SELECT id,user_id,name,scopes,targets,expires,revoked,last_used FROM api_tokens ORDER BY id`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	list := []TokenInfo{}
	for rows.Next() {
		var t TokenInfo
		var scopes, targets string
		if e = rows.Scan(&t.ID, &t.UserID, &t.Name, &scopes, &targets, &t.Expires, &t.Revoked, &t.LastUsed); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(scopes), &t.Scopes); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(targets), &t.Targets); e != nil {
			return nil, e
		}
		list = append(list, t)
	}
	return list, rows.Err()
}

type Principal struct {
	User            User
	TokenID         string
	Scopes, Targets []string
	CSRF            string
	Session         string
}

func (p Principal) Actor() string {
	if p.TokenID != "" {
		return p.User.Username + "/token:" + p.TokenID
	}
	return p.User.Username
}
func (p Principal) Allows(scope, target string) bool {
	if p.User.Disabled || scope == "targets:write" && p.User.Role != "admin" {
		return false
	}
	if p.User.Role == "reader" && !strings.HasSuffix(scope, ":read") {
		return false
	}
	if p.TokenID == "" {
		return true
	}
	return contains(p.Scopes, scope) && (target == "" || contains(p.Targets, target) || contains(p.Targets, "*"))
}
func (p Principal) Admin() bool { return p.TokenID == "" && p.User.Role == "admin" && !p.User.Disabled }
func (s *Store) AuthenticateToken(raw string) (Principal, error) {
	var p Principal
	parts := strings.Split(raw, "_")
	if len(parts) != 3 || parts[0] != "scron" {
		return p, ErrForbidden
	}
	var hash, userID, scopes, targets string
	var expiry int64
	var revoked bool
	e := s.DB.QueryRow(`SELECT digest,user_id,scopes,targets,expires,revoked FROM api_tokens WHERE id=?`, parts[1]).Scan(&hash, &userID, &scopes, &targets, &expiry, &revoked)
	if e != nil || revoked || expiry <= nowMS() || subtle.ConstantTimeCompare([]byte(hash), []byte(digest(raw))) != 1 {
		return p, ErrForbidden
	}
	p.User, e = userFrom(s.DB, "id", userID)
	if e != nil || p.User.Disabled {
		return p, ErrForbidden
	}
	p.TokenID = parts[1]
	if json.Unmarshal([]byte(scopes), &p.Scopes) != nil || json.Unmarshal([]byte(targets), &p.Targets) != nil {
		return p, ErrForbidden
	}
	_, e = s.DB.Exec(`UPDATE api_tokens SET last_used=? WHERE id=?`, nowMS(), p.TokenID)
	return p, e
}
func (s *Store) NewSession(u User) (string, string, error) {
	raw := hex.EncodeToString(randomBytes(32))
	csrf := hex.EncodeToString(randomBytes(32))
	now := nowMS()
	_, e := s.DB.Exec(`INSERT INTO sessions VALUES(?,?,?,?,?,?)`, digest(raw), u.ID, csrf, now, now, now+int64(12*time.Hour/time.Millisecond))
	return raw, csrf, e
}
func (s *Store) AuthenticateSession(raw string) (Principal, error) {
	var p Principal
	var userID string
	var created, last, expiry int64
	e := s.DB.QueryRow(`SELECT user_id,csrf,created,last_seen,expires FROM sessions WHERE digest=?`, digest(raw)).Scan(&userID, &p.CSRF, &created, &last, &expiry)
	if e != nil || expiry <= nowMS() || last < nowMS()-int64(30*time.Minute/time.Millisecond) {
		return p, ErrForbidden
	}
	p.User, e = userFrom(s.DB, "id", userID)
	if e != nil || p.User.Disabled || p.User.Service {
		return p, ErrForbidden
	}
	p.Session = digest(raw)
	_, e = s.DB.Exec(`UPDATE sessions SET last_seen=? WHERE digest=?`, nowMS(), p.Session)
	return p, e
}
func (s *Store) RevokeToken(id, actor string) error {
	return transaction(s.DB, func(tx *sql.Tx) error {
		result, e := tx.Exec(`UPDATE api_tokens SET revoked=1 WHERE id=?`, id)
		if e != nil {
			return e
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			return sql.ErrNoRows
		}
		return audit(tx, actor, "token.revoke", id, "")
	})
}
func (s *Store) ResetAccess() error {
	return transaction(s.DB, func(tx *sql.Tx) error {
		if _, e := tx.Exec(`DELETE FROM sessions`); e != nil {
			return e
		}
		if _, e := tx.Exec(`UPDATE api_tokens SET revoked=1`); e != nil {
			return e
		}
		return audit(tx, "local-cli", "auth.reset", "all", "")
	})
}
func (s *Store) Reencrypt(v *Vault) error {
	return transaction(s.DB, func(tx *sql.Tx) error {
		rows, e := tx.Query(`SELECT id,encrypted FROM credentials`)
		if e != nil {
			return e
		}
		values := map[string]string{}
		for rows.Next() {
			var id, encrypted string
			if e = rows.Scan(&id, &encrypted); e != nil {
				rows.Close()
				return e
			}
			plain, err := v.Open(id, encrypted)
			if err != nil {
				rows.Close()
				return err
			}
			sealed, err := v.Seal(id, plain)
			if err != nil {
				rows.Close()
				return err
			}
			values[id] = sealed
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		for id, value := range values {
			if _, e = tx.Exec(`UPDATE credentials SET encrypted=? WHERE id=?`, value, id); e != nil {
				return e
			}
		}
		return audit(tx, "local-cli", "credentials.reencrypt", v.Active, fmt.Sprintf("%d credentials", len(values)))
	})
}
