package db

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"doriyamedia/internal/models"

	_ "modernc.org/sqlite"
)

type Store struct {
	DB *sql.DB
}

// legacyDBName is the database file used before the DoriyaMedia rename.
const legacyDBName = "doriya.db"

// MigrateLegacyDB renames the pre-rename database (data/doriya.db, plus its
// WAL sidecars) to the current name so an existing library keeps working.
// It only acts when the new file is missing, and never overwrites data.
func MigrateLegacyDB(path string) {
	if filepath.Base(path) == legacyDBName {
		return
	}
	if _, err := os.Stat(path); err == nil {
		return // already migrated
	}
	legacy := filepath.Join(filepath.Dir(path), legacyDBName)
	if _, err := os.Stat(legacy); err != nil {
		return // nothing to migrate
	}
	moved := 0
	for _, suffix := range []string{"", "-wal", "-shm"} {
		from, to := legacy+suffix, path+suffix
		if _, err := os.Stat(from); err != nil {
			continue
		}
		if err := os.Rename(from, to); err != nil {
			log.Printf("[db] 旧库自动改名失败(%v),继续使用原文件: %s", err, from)
			return
		}
		moved++
	}
	if moved > 0 {
		log.Printf("[db] 已把旧数据库 %s 迁移为 %s(原有媒体库数据保留)", legacyDBName, filepath.Base(path))
	}
}

func Open(path string) (*Store, error) {
	MigrateLegacyDB(path)
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A small pool rather than exactly one connection. WAL mode is what makes
	// this safe — readers do not block the writer — and busy_timeout handles the
	// one case that still contends (two writers). One connection was the earlier
	// setting, and it is what a long library scan tripped over: after a walk that
	// takes minutes, a read could fail with "unable to open database file" while
	// every other query on the same handle still worked, which points at the pool
	// rather than the file. With room to open a second connection, a statement
	// that cannot get a usable one has another to try.
	db.SetMaxOpenConns(4)
	s := &Store{DB: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT UNIQUE NOT NULL,
			pass_hash TEXT NOT NULL,
			salt TEXT NOT NULL,
			role TEXT NOT NULL DEFAULT 'user',
			status TEXT NOT NULL DEFAULT 'active',
			email TEXT NOT NULL DEFAULT '',
			email_verified INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL,
			last_login_at TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			token TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			expires_at TIMESTAMP NOT NULL
		)`,
		// One-shot tokens mailed to a user: kind 'verify' proves an address,
		// kind 'reset' authorises a new password. Rows cascade with the account,
		// and only one of each kind is kept per user, so asking for a new link
		// invalidates the previous one.
		`CREATE TABLE IF NOT EXISTS auth_tokens (
			token TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			kind TEXT NOT NULL,
			expires_at TIMESTAMP NOT NULL,
			created_at TIMESTAMP NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_auth_tokens_user ON auth_tokens(user_id, kind)`,
		`CREATE TABLE IF NOT EXISTS sources (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			type TEXT NOT NULL,
			config TEXT NOT NULL DEFAULT '{}',
			created_at TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS libraries (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			type TEXT NOT NULL,
			source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
			path TEXT NOT NULL DEFAULT '/',
			providers TEXT NOT NULL DEFAULT '["nfo","tmdb"]',
			created_at TIMESTAMP NOT NULL
		)`,
		// The places one library scans. A library used to be bound to a single
		// source; the row in `libraries` still names the first of these, so every
		// caller that only knows about one source keeps working.
		`CREATE TABLE IF NOT EXISTS library_sources (
			library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
			source_id INTEGER NOT NULL REFERENCES sources(id) ON DELETE CASCADE,
			path TEXT NOT NULL DEFAULT '/',
			PRIMARY KEY (library_id, source_id, path)
		)`,
		`CREATE TABLE IF NOT EXISTS items (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
			type TEXT NOT NULL,
			parent_id INTEGER REFERENCES items(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			original_title TEXT DEFAULT '',
			year INTEGER DEFAULT 0,
			tmdb_id INTEGER DEFAULT 0,
			bgm_id INTEGER DEFAULT 0,
			overview TEXT DEFAULT '',
			poster_path TEXT DEFAULT '',
			backdrop_path TEXT DEFAULT '',
			still_path TEXT DEFAULT '',
			rating REAL DEFAULT 0,
			genres TEXT DEFAULT '[]',
			season INTEGER DEFAULT 0,
			episode INTEGER DEFAULT 0,
			air_date TEXT DEFAULT '',
			source_type TEXT NOT NULL DEFAULT '',
			added_at TIMESTAMP NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_items_library ON items(library_id, type)`,
		`CREATE INDEX IF NOT EXISTS idx_items_parent ON items(parent_id)`,
		`CREATE TABLE IF NOT EXISTS files (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			item_id INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
			source_id INTEGER NOT NULL,
			path TEXT NOT NULL,
			size INTEGER DEFAULT 0,
			mtime INTEGER DEFAULT 0,
			probe TEXT DEFAULT '',
			added_at TIMESTAMP NOT NULL,
			UNIQUE(source_id, path)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_files_item ON files(item_id)`,
		`CREATE TABLE IF NOT EXISTS subtitles (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
			path TEXT DEFAULT '',
			lang TEXT DEFAULT '',
			title TEXT DEFAULT '',
			format TEXT DEFAULT '',
			embedded INTEGER DEFAULT 0,
			stream_idx INTEGER DEFAULT 0,
			convertible INTEGER DEFAULT 1,
			UNIQUE(file_id, path, stream_idx)
		)`,
		`CREATE TABLE IF NOT EXISTS progress (
			user_id INTEGER NOT NULL,
			file_id INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
			item_id INTEGER NOT NULL,
			position REAL NOT NULL DEFAULT 0,
			duration REAL NOT NULL DEFAULT 0,
			updated_at TIMESTAMP NOT NULL,
			PRIMARY KEY (user_id, file_id)
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.DB.Exec(stmt); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	if err := s.migrateFilesUnique(); err != nil {
		return err
	}
	if err := s.migrateLibrarySources(); err != nil {
		return err
	}
	if err := s.migrateItemSourceTypes(); err != nil {
		return err
	}
	return s.migrateUsers()
}

// migrateLibrarySources records where every existing library scans. A library
// from before multi-source support has exactly one place, named by its own
// source_id/path; after this its library_sources row says the same thing, and
// the scan reads the table instead of the column.
func (s *Store) migrateLibrarySources() error {
	_, err := s.DB.Exec(`INSERT OR IGNORE INTO library_sources (library_id, source_id, path)
		SELECT id, source_id, path FROM libraries WHERE source_id > 0 AND name NOT LIKE '__browse__%'`)
	return err
}

// migrateUsers adds the columns a database from before multi-user support is
// missing. Existing rows keep working: the single account such a database had
// was created as an admin by the setup flow, so it stays an active admin.
//
// The email columns land empty, which is what exempts long-standing accounts
// from the address check — an owner who turns verification on must not find
// their own account locked out behind a mailbox it never had.
func (s *Store) migrateUsers() error {
	cols := []struct{ name, ddl string }{
		{"status", `ALTER TABLE users ADD COLUMN status TEXT NOT NULL DEFAULT 'active'`},
		{"last_login_at", `ALTER TABLE users ADD COLUMN last_login_at TIMESTAMP`},
		{"email", `ALTER TABLE users ADD COLUMN email TEXT NOT NULL DEFAULT ''`},
		{"email_verified", `ALTER TABLE users ADD COLUMN email_verified INTEGER NOT NULL DEFAULT 0`},
	}
	for _, c := range cols {
		if err := s.addColumn("users", c.name, c.ddl); err != nil {
			return err
		}
	}
	return nil
}

// migrateItemSourceTypes adds the column the UI badges read and fills it for
// libraries that already have rows. A rescan would fill it too, but a badge
// disappearing until the next scan is not a good first impression, and the
// backfill is three UPDATEs per library.
func (s *Store) migrateItemSourceTypes() error {
	has, err := s.hasColumn("items", "source_type")
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	if _, err := s.DB.Exec(`ALTER TABLE items ADD COLUMN source_type TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("items.source_type: %w", err)
	}
	rows, err := s.DB.Query(`SELECT id FROM libraries`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.RefreshItemSourceTypes(id); err != nil {
			return err
		}
	}
	return nil
}

// RefreshItemSourceTypes recomputes items.source_type for one library.
//
// The value is the source *type* every file of the item comes from — an
// episode's own files, a season's episodes' files, a show's season episodes'
// files. An item whose files span two kinds of source gets the empty string:
// there is no single badge that is true for it, and the UI shows none.
//
// It runs after every organize pass, so a rescan after moving a show to the
// bilibili source picks the badge up (and dropping the source drops it), and it
// is also what the one-time migration calls for libraries that predate the
// column.
func (s *Store) RefreshItemSourceTypes(libraryID int64) error {
	stmts := []string{
		// Episodes and films hold the files themselves.
		`UPDATE items SET source_type = COALESCE((
			SELECT MIN(src.type) FROM files f JOIN sources src ON src.id = f.source_id
			WHERE f.item_id = items.id
			HAVING COUNT(DISTINCT src.type) = 1
		), '') WHERE library_id = ? AND type IN ('movie','episode')`,
		// A season's files are its episodes'.
		`UPDATE items SET source_type = COALESCE((
			SELECT MIN(src.type) FROM items e
			JOIN files f ON f.item_id = e.id JOIN sources src ON src.id = f.source_id
			WHERE e.parent_id = items.id
			HAVING COUNT(DISTINCT src.type) = 1
		), '') WHERE library_id = ? AND type = 'season'`,
		// A show's files are its seasons' episodes'.
		`UPDATE items SET source_type = COALESCE((
			SELECT MIN(src.type) FROM items se
			JOIN items e ON e.parent_id = se.id
			JOIN files f ON f.item_id = e.id JOIN sources src ON src.id = f.source_id
			WHERE se.parent_id = items.id
			HAVING COUNT(DISTINCT src.type) = 1
		), '') WHERE library_id = ? AND type = 'show'`,
	}
	for _, stmt := range stmts {
		if _, err := s.DB.Exec(stmt, libraryID); err != nil {
			return err
		}
	}
	return nil
}

// addColumn adds one column to an existing table when it predates it. SQLite
// has no "ADD COLUMN IF NOT EXISTS", and migrate() runs on every start, so the
// current columns are read first.
func (s *Store) addColumn(table, column, ddl string) error {
	has, err := s.hasColumn(table, column)
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	if _, err := s.DB.Exec(ddl); err != nil {
		return fmt.Errorf("%s.%s: %w", table, column, err)
	}
	return nil
}

// migrateFilesUnique upgrades the files table to "one row per (source, path)".
// The old schema was UNIQUE(item_id, source_id, path), which meant a rescan
// could never re-parent a file (the parse fix would duplicate rows instead of
// moving them) — so collapse duplicates, keeping the newest binding, then add
// the unique index the new UpsertFile conflicts on.
func (s *Store) migrateFilesUnique() error {
	if _, err := s.DB.Exec(`DELETE FROM files WHERE id NOT IN (SELECT MAX(id) FROM files GROUP BY source_id, path)`); err != nil {
		return fmt.Errorf("dedupe files: %w", err)
	}
	if _, err := s.DB.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_files_source_path ON files(source_id, path)`); err != nil {
		return fmt.Errorf("files index: %w", err)
	}
	// Bangumi id column for databases created before the bgm provider existed.
	has, err := s.hasColumn("items", "bgm_id")
	if err != nil {
		return err
	}
	if !has {
		if _, err := s.DB.Exec(`ALTER TABLE items ADD COLUMN bgm_id INTEGER DEFAULT 0`); err != nil {
			return fmt.Errorf("items.bgm_id: %w", err)
		}
	}
	return nil
}

func (s *Store) hasColumn(table, column string) (bool, error) {
	rows, err := s.DB.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if strings.EqualFold(name, column) {
			return true, nil
		}
	}
	return false, rows.Err()
}

// ---------- users ----------

// userCols is the column list every user query scans, in the order scanUser
// reads them, so adding a column means touching those two places only.
const userCols = `id, name, pass_hash, salt, role, status, email, email_verified, created_at, last_login_at`

func scanUser(row interface{ Scan(...any) error }) (*models.User, error) {
	u := &models.User{}
	var last sql.NullTime
	var verified int
	if err := row.Scan(&u.ID, &u.Name, &u.PassHash, &u.Salt, &u.Role, &u.Status,
		&u.Email, &verified, &u.CreatedAt, &last); err != nil {
		return nil, err
	}
	u.EmailVerified = verified != 0
	if last.Valid {
		t := last.Time
		u.LastLogin = &t
	}
	return u, nil
}

func (s *Store) UserCount() (int, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// ActiveAdminCount counts accounts that can still administer the server. A
// disabled or pending admin does not count, which is what stops the last usable
// admin from being demoted, disabled or deleted out from under the server.
func (s *Store) ActiveAdminCount() (int, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM users WHERE role = ? AND status = ?`,
		models.RoleAdmin, models.StatusActive).Scan(&n)
	return n, err
}

// ListUsers returns every account, oldest first, for the admin user table.
func (s *Store) ListUsers() ([]models.User, error) {
	rows, err := s.DB.Query(`SELECT ` + userCols + ` FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

func (s *Store) CreateUser(u *models.User) error {
	if u.Role == "" {
		u.Role = models.RoleUser
	}
	if u.Status == "" {
		u.Status = models.StatusActive
	}
	res, err := s.DB.Exec(`INSERT INTO users (name, pass_hash, salt, role, status, email, email_verified, created_at) VALUES (?,?,?,?,?,?,?,?)`,
		u.Name, u.PassHash, u.Salt, u.Role, u.Status, u.Email, boolToInt(u.EmailVerified), time.Now())
	if err != nil {
		return err
	}
	u.ID, _ = res.LastInsertId()
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// UserByName looks a name up case-insensitively: SQLite's default = is
// case-sensitive, which would let someone register "Admin" next to an existing
// "admin" and sign in as a convincing impostor in the UI.
func (s *Store) UserByName(name string) (*models.User, error) {
	return scanUser(s.DB.QueryRow(`SELECT `+userCols+` FROM users WHERE name = ? COLLATE NOCASE`, name))
}

// UserByLogin finds the account behind a sign-in form or a "forgot password"
// box, where the person may type either their name or their address.
func (s *Store) UserByLogin(nameOrEmail string) (*models.User, error) {
	return scanUser(s.DB.QueryRow(
		`SELECT `+userCols+` FROM users WHERE name = ? COLLATE NOCASE OR (email <> '' AND email = ? COLLATE NOCASE)`,
		nameOrEmail, nameOrEmail))
}

func (s *Store) UserByID(id int64) (*models.User, error) {
	return scanUser(s.DB.QueryRow(`SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

// NameTaken reports whether another account already uses that name, so the API
// can answer with a readable message instead of the UNIQUE constraint's text.
// exceptID is the account being edited (0 when creating a new one). It is
// case-insensitive for the same reason as UserByName.
func (s *Store) NameTaken(name string, exceptID int64) (bool, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM users WHERE name = ? COLLATE NOCASE AND id <> ?`, name, exceptID).Scan(&n)
	return n > 0, err
}

// EmailTaken reports whether another account already claims that address. An
// address identifies a person for password resets, so two accounts must not
// share one.
func (s *Store) EmailTaken(email string, exceptID int64) (bool, error) {
	var n int
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM users WHERE email <> '' AND email = ? COLLATE NOCASE AND id <> ?`,
		email, exceptID).Scan(&n)
	return n > 0, err
}

func (s *Store) RenameUser(id int64, name string) error {
	_, err := s.DB.Exec(`UPDATE users SET name = ? WHERE id = ?`, name, id)
	return err
}

func (s *Store) UpdateUserRole(id int64, role string) error {
	_, err := s.DB.Exec(`UPDATE users SET role = ? WHERE id = ?`, role, id)
	return err
}

func (s *Store) UpdateUserStatus(id int64, status string) error {
	_, err := s.DB.Exec(`UPDATE users SET status = ? WHERE id = ?`, status, id)
	return err
}

func (s *Store) UpdateUserPassword(id int64, hash, salt string) error {
	_, err := s.DB.Exec(`UPDATE users SET pass_hash = ?, salt = ? WHERE id = ?`, hash, salt, id)
	return err
}

func (s *Store) TouchLastLogin(id int64, t time.Time) error {
	_, err := s.DB.Exec(`UPDATE users SET last_login_at = ? WHERE id = ?`, t, id)
	return err
}

// SetUserEmail stores an address and whether it has been proven. Changing the
// address clears the flag unless the caller explicitly vouches for it (an admin
// marking an account verified, or a completed verification link).
func (s *Store) SetUserEmail(id int64, email string, verified bool) error {
	_, err := s.DB.Exec(`UPDATE users SET email = ?, email_verified = ? WHERE id = ?`,
		email, boolToInt(verified), id)
	return err
}

// ---------- one-shot auth tokens ----------

// Auth token kinds. Each account keeps at most one token per kind, so asking
// for a new link invalidates the previous one.
const (
	TokenVerifyEmail   = "verify"
	TokenResetPassword = "reset"
)

// CreateAuthToken issues token, replacing any outstanding one of the same kind
// for that user. Returning the old link useless is the point: a mailbox full of
// half-used links should not be a set of live keys.
func (s *Store) CreateAuthToken(token string, userID int64, kind string, expires time.Time) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM auth_tokens WHERE user_id = ? AND kind = ?`, userID, kind); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO auth_tokens (token, user_id, kind, expires_at, created_at) VALUES (?,?,?,?,?)`,
		token, userID, kind, expires, time.Now()); err != nil {
		return err
	}
	return tx.Commit()
}

// AuthTokenUserID resolves a token of the given kind, without judging whether
// it has expired — the caller decides what to tell the user.
func (s *Store) AuthTokenUserID(token, kind string) (int64, time.Time, error) {
	var userID int64
	var expires time.Time
	err := s.DB.QueryRow(`SELECT user_id, expires_at FROM auth_tokens WHERE token = ? AND kind = ?`, token, kind).
		Scan(&userID, &expires)
	return userID, expires, err
}

func (s *Store) DeleteAuthToken(token string) error {
	_, err := s.DB.Exec(`DELETE FROM auth_tokens WHERE token = ?`, token)
	return err
}

// DeleteAuthTokens drops a user's outstanding tokens, used after a reset so the
// link that was just followed cannot be replayed.
func (s *Store) DeleteAuthTokens(userID int64, kind string) error {
	_, err := s.DB.Exec(`DELETE FROM auth_tokens WHERE user_id = ? AND kind = ?`, userID, kind)
	return err
}

// PruneAuthTokens drops tokens that have expired.
func (s *Store) PruneAuthTokens() error {
	_, err := s.DB.Exec(`DELETE FROM auth_tokens WHERE expires_at < ?`, time.Now())
	return err
}

// DeleteUser removes the account, its sessions (via the foreign key cascade) and
// its playback progress. Progress has no foreign key to users, so rows would
// otherwise survive and attach themselves to whatever account reuses the id.
func (s *Store) DeleteUser(id int64) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM sessions WHERE user_id = ?`,
		`DELETE FROM progress WHERE user_id = ?`,
		`DELETE FROM users WHERE id = ?`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteUserSessions signs every device of one account out, without touching
// the account itself. excludeToken keeps the caller's own session alive.
func (s *Store) DeleteUserSessions(userID int64, excludeToken string) error {
	if excludeToken != "" {
		_, err := s.DB.Exec(`DELETE FROM sessions WHERE user_id = ? AND token <> ?`, userID, excludeToken)
		return err
	}
	_, err := s.DB.Exec(`DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

// PruneSessions drops sessions that have already expired, so the table does not
// grow for the lifetime of the server.
func (s *Store) PruneSessions() error {
	_, err := s.DB.Exec(`DELETE FROM sessions WHERE expires_at < ?`, time.Now())
	return err
}

// ---------- sessions ----------

func (s *Store) CreateSession(token string, userID int64, expires time.Time) error {
	_, err := s.DB.Exec(`INSERT INTO sessions (token, user_id, expires_at) VALUES (?,?,?)`, token, userID, expires)
	return err
}

func (s *Store) SessionByToken(token string) (*models.Session, error) {
	sess := &models.Session{}
	err := s.DB.QueryRow(`SELECT token, user_id, expires_at FROM sessions WHERE token = ?`, token).
		Scan(&sess.Token, &sess.UserID, &sess.ExpiresAt)
	if err != nil {
		return nil, err
	}
	return sess, nil
}

func (s *Store) DeleteSession(token string) error {
	_, err := s.DB.Exec(`DELETE FROM sessions WHERE token = ?`, token)
	return err
}

// ---------- sources ----------

func (s *Store) CreateSource(src *models.Source) error {
	res, err := s.DB.Exec(`INSERT INTO sources (name, type, config, created_at) VALUES (?,?,?,?)`,
		src.Name, src.Type, src.Config, time.Now())
	if err != nil {
		return err
	}
	src.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) UpdateSource(src *models.Source) error {
	_, err := s.DB.Exec(`UPDATE sources SET name=?, type=?, config=? WHERE id=?`, src.Name, src.Type, src.Config, src.ID)
	return err
}

func (s *Store) DeleteSource(id int64) error {
	// `libraries.source_id` still carries ON DELETE CASCADE, so a library whose
	// *first* source goes would go with it. When the library scans somewhere else
	// as well, point its own source at that place first: losing one of several
	// sources is not losing the library. (A library that has no other source is
	// still removed with it — that is the documented behaviour.)
	if _, err := s.DB.Exec(`UPDATE libraries SET source_id = (
			SELECT ls.source_id FROM library_sources ls
			WHERE ls.library_id = libraries.id AND ls.source_id <> ?
			ORDER BY ls.rowid LIMIT 1),
		path = COALESCE((
			SELECT ls.path FROM library_sources ls
			WHERE ls.library_id = libraries.id AND ls.source_id <> ?
			ORDER BY ls.rowid LIMIT 1), path)
		WHERE source_id = ? AND EXISTS (
			SELECT 1 FROM library_sources ls
			WHERE ls.library_id = libraries.id AND ls.source_id <> ?)`, id, id, id, id); err != nil {
		return err
	}
	_, err := s.DB.Exec(`DELETE FROM sources WHERE id=?`, id)
	return err
}

func (s *Store) SourceByID(id int64) (*models.Source, error) {
	src := &models.Source{}
	err := s.DB.QueryRow(`SELECT id, name, type, config, created_at FROM sources WHERE id=?`, id).
		Scan(&src.ID, &src.Name, &src.Type, &src.Config, &src.CreatedAt)
	if err != nil {
		return nil, err
	}
	return src, nil
}

func (s *Store) Sources() ([]models.Source, error) {
	rows, err := s.DB.Query(`SELECT id, name, type, config, created_at FROM sources ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Source
	for rows.Next() {
		var src models.Source
		if err := rows.Scan(&src.ID, &src.Name, &src.Type, &src.Config, &src.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

// ---------- libraries ----------

// CreateLibrary inserts a library, and records where it scans. The first source
// is the library's own source_id/path, which is what every older caller reads.
func (s *Store) CreateLibrary(lib *models.Library) error {
	res, err := s.DB.Exec(`INSERT INTO libraries (name, type, source_id, path, providers, created_at) VALUES (?,?,?,?,?,?)`,
		lib.Name, lib.Type, lib.SourceID, lib.Path, lib.Providers, time.Now())
	if err != nil {
		return err
	}
	lib.ID, _ = res.LastInsertId()
	if lib.SourceID > 0 {
		_, err = s.DB.Exec(`INSERT OR IGNORE INTO library_sources (library_id, source_id, path) VALUES (?,?,?)`,
			lib.ID, lib.SourceID, lib.Path)
	}
	return err
}

func (s *Store) UpdateLibrary(lib *models.Library) error {
	_, err := s.DB.Exec(`UPDATE libraries SET name=?, type=?, source_id=?, path=?, providers=? WHERE id=?`,
		lib.Name, lib.Type, lib.SourceID, lib.Path, lib.Providers, lib.ID)
	return err
}

// LibrarySources returns the places a library scans, in the order they were
// added. A library from before this existed has its single source_id/path
// recorded by the migration, so this is never empty for a usable library.
func (s *Store) LibrarySources(libraryID int64) ([]models.LibrarySource, error) {
	rows, err := s.DB.Query(`SELECT library_id, source_id, path FROM library_sources
		WHERE library_id=? ORDER BY rowid`, libraryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.LibrarySource
	for rows.Next() {
		var ls models.LibrarySource
		if err := rows.Scan(&ls.LibraryID, &ls.SourceID, &ls.Path); err != nil {
			return nil, err
		}
		out = append(out, ls)
	}
	return out, rows.Err()
}

// SetLibrarySources replaces a library's sources in one transaction, and keeps
// the library's own source_id/path pointing at the first of them — that pair is
// what a stale client, the home page and the browse scratchpad still read.
func (s *Store) SetLibrarySources(libraryID int64, list []models.LibrarySource) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM library_sources WHERE library_id=?`, libraryID); err != nil {
		return err
	}
	for _, ls := range list {
		if ls.SourceID <= 0 {
			continue
		}
		path := ls.Path
		if strings.TrimSpace(path) == "" {
			path = "/"
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO library_sources (library_id, source_id, path) VALUES (?,?,?)`,
			libraryID, ls.SourceID, path); err != nil {
			return err
		}
	}
	if len(list) > 0 && list[0].SourceID > 0 {
		path := list[0].Path
		if strings.TrimSpace(path) == "" {
			path = "/"
		}
		if _, err := tx.Exec(`UPDATE libraries SET source_id=?, path=? WHERE id=?`,
			list[0].SourceID, path, libraryID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// attachSources fills in Library.Sources for a set of libraries with one query.
func (s *Store) attachSources(libs []models.Library) {
	if len(libs) == 0 {
		return
	}
	byLib := map[int64][]models.LibrarySource{}
	rows, err := s.DB.Query(`SELECT library_id, source_id, path FROM library_sources ORDER BY rowid`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var ls models.LibrarySource
		if err := rows.Scan(&ls.LibraryID, &ls.SourceID, &ls.Path); err != nil {
			return
		}
		byLib[ls.LibraryID] = append(byLib[ls.LibraryID], ls)
	}
	for i := range libs {
		libs[i].Sources = byLib[libs[i].ID]
		// A library with no recorded sources still scans where its own row says.
		if len(libs[i].Sources) == 0 && libs[i].SourceID > 0 {
			libs[i].Sources = []models.LibrarySource{{
				LibraryID: libs[i].ID, SourceID: libs[i].SourceID, Path: libs[i].Path,
			}}
		}
	}
}

func (s *Store) DeleteLibrary(id int64) error {
	_, err := s.DB.Exec(`DELETE FROM libraries WHERE id=?`, id)
	return err
}

func (s *Store) LibraryByID(id int64) (*models.Library, error) {
	lib := &models.Library{}
	err := s.DB.QueryRow(`SELECT id, name, type, source_id, path, providers, created_at FROM libraries WHERE id=?`, id).
		Scan(&lib.ID, &lib.Name, &lib.Type, &lib.SourceID, &lib.Path, &lib.Providers, &lib.CreatedAt)
	if err != nil {
		return nil, err
	}
	libs := []models.Library{*lib}
	s.attachSources(libs)
	*lib = libs[0]
	return lib, nil
}

// EnsureBrowseLibrary returns the hidden scratchpad library used by the file
// browser (one per source). Items played directly from the browser land here.
func (s *Store) EnsureBrowseLibrary(sourceID int64) (*models.Library, error) {
	name := fmt.Sprintf("__browse__-%d", sourceID)
	lib := &models.Library{}
	err := s.DB.QueryRow(`SELECT id, name, type, source_id, path, providers, created_at
		FROM libraries WHERE name = ? AND source_id = ?`, name, sourceID).
		Scan(&lib.ID, &lib.Name, &lib.Type, &lib.SourceID, &lib.Path, &lib.Providers, &lib.CreatedAt)
	if err == nil {
		return lib, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	lib = &models.Library{Name: name, Type: "movie", SourceID: sourceID, Path: "/", Providers: "[]"}
	if err := s.CreateLibrary(lib); err != nil {
		return nil, err
	}
	return lib, nil
}

func (s *Store) Libraries() ([]models.Library, error) {
	rows, err := s.DB.Query(`SELECT id, name, type, source_id, path, providers, created_at FROM libraries ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Library
	for rows.Next() {
		var lib models.Library
		if err := rows.Scan(&lib.ID, &lib.Name, &lib.Type, &lib.SourceID, &lib.Path, &lib.Providers, &lib.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, lib)
	}
	s.attachSources(out)
	return out, rows.Err()
}

// ---------- items ----------

func (s *Store) InsertItem(it *models.Item) error {
	res, err := s.execRetryResult(`INSERT INTO items
		(library_id, type, parent_id, name, original_title, year, tmdb_id, bgm_id, overview, poster_path, backdrop_path, still_path, rating, genres, season, episode, air_date, source_type, added_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		it.LibraryID, it.Type, it.ParentID, it.Name, it.OriginalTitle, it.Year, it.TMDBID, it.BGMID, it.Overview,
		it.PosterPath, it.BackdropPath, it.StillPath, it.Rating, it.Genres, it.Season, it.Episode, it.AirDate, it.SourceType, time.Now())
	if err != nil {
		return err
	}
	it.ID, _ = res.LastInsertId()
	return nil
}

func (s *Store) UpdateItemMeta(it *models.Item) error {
	_, err := s.DB.Exec(`UPDATE items SET name=?, original_title=?, year=?, tmdb_id=?, bgm_id=?, overview=?,
		poster_path=?, backdrop_path=?, still_path=?, rating=?, genres=?, season=?, episode=?, air_date=? WHERE id=?`,
		it.Name, it.OriginalTitle, it.Year, it.TMDBID, it.BGMID, it.Overview,
		it.PosterPath, it.BackdropPath, it.StillPath, it.Rating, it.Genres, it.Season, it.Episode, it.AirDate, it.ID)
	return err
}

func (s *Store) DeleteItem(id int64) error {
	_, err := s.DB.Exec(`DELETE FROM items WHERE id=?`, id)
	return err
}

// itemCols is the column list every item query scans, in the order scanItems
// reads it. Adding an item field means this line, scanItems and any per-row
// reader (ItemByID) — not five nearly identical SELECTs.
const itemCols = `id, library_id, type, parent_id, name, original_title, year, tmdb_id, bgm_id, overview,
	poster_path, backdrop_path, still_path, rating, genres, season, episode, air_date, source_type, added_at`

func (s *Store) ItemByID(id int64) (*models.Item, error) {
	it := &models.Item{}
	err := s.DB.QueryRow(`SELECT `+itemCols+` FROM items WHERE id=?`, id).
		Scan(&it.ID, &it.LibraryID, &it.Type, &it.ParentID, &it.Name, &it.OriginalTitle, &it.Year, &it.TMDBID, &it.BGMID,
			&it.Overview, &it.PosterPath, &it.BackdropPath, &it.StillPath, &it.Rating, &it.Genres,
			&it.Season, &it.Episode, &it.AirDate, &it.SourceType, &it.AddedAt)
	if err != nil {
		return nil, err
	}
	return it, nil
}

// ItemChildren returns child items ordered by season/episode or name.
func (s *Store) ItemChildren(parentID int64) ([]models.Item, error) {
	rows, err := s.DB.Query(`SELECT `+itemCols+` FROM items WHERE parent_id=? ORDER BY season, episode, name`, parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanItems(rows)
}

// ItemsByLibrary lists a library's items. rootsOnly keeps the shelf to what a
// library actually shows — films and series — and leaves seasons and episodes to
// the pages that drill into them. Without it a series library lists every episode
// it holds by added_at, so a freshly scanned library opens on a wall of episodes
// with its series buried behind them.
func (s *Store) ItemsByLibrary(libraryID int64, typ string, search string, sort string, rootsOnly bool, offset, limit int) ([]models.Item, int, error) {
	where := []string{"library_id = ?"}
	args := []any{libraryID}
	if typ != "" {
		where = append(where, "type = ?")
		args = append(args, typ)
	}
	if rootsOnly {
		where = append(where, "parent_id IS NULL")
	}
	if search != "" {
		where = append(where, "name LIKE ?")
		args = append(args, "%"+search+"%")
	}
	w := strings.Join(where, " AND ")
	orderBy := "added_at DESC"
	switch sort {
	case "name":
		orderBy = "name COLLATE NOCASE ASC"
	case "year":
		orderBy = "year DESC, name COLLATE NOCASE ASC"
	case "rating":
		orderBy = "rating DESC, name COLLATE NOCASE ASC"
	}
	var total int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM items WHERE `+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	q := `SELECT ` + itemCols + ` FROM items WHERE ` + w + ` ORDER BY ` + orderBy + ` LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	items, err := s.queryItems(q, args...)
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

func (s *Store) RecentItems(typ string, limit int) ([]models.Item, error) {
	// exclude the hidden file-browser scratchpad libraries from home rows
	rows, err := s.DB.Query(`SELECT `+itemCols+`
		FROM items WHERE type=? AND library_id NOT IN (SELECT id FROM libraries WHERE name LIKE '__browse__%')
		ORDER BY added_at DESC LIMIT ?`, typ, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanItems(rows)
}

// CountItems totals one item type across all visible libraries (home "查看全部").
func (s *Store) CountItems(typ string) (int64, error) {
	var n int64
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM items
		WHERE type=? AND library_id NOT IN (SELECT id FROM libraries WHERE name LIKE '__browse__%')`, typ).Scan(&n)
	return n, err
}

// ItemsNeedingIdent returns items of given types without tmdb id and without custom names,
// used by the metadata enricher.
func (s *Store) ItemsNeedingIdent(types []string) ([]models.Item, error) {
	q := `SELECT ` + itemCols + `
		FROM items WHERE tmdb_id = 0 AND bgm_id = 0 AND overview = '' AND type IN (` + placeholders(len(types)) + `)`
	args := make([]any, 0, len(types))
	for _, t := range types {
		args = append(args, t)
	}
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanItems(rows)
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func scanItems(rows *sql.Rows) ([]models.Item, error) {
	var out []models.Item
	for rows.Next() {
		var it models.Item
		if err := rows.Scan(&it.ID, &it.LibraryID, &it.Type, &it.ParentID, &it.Name, &it.OriginalTitle, &it.Year,
			&it.TMDBID, &it.BGMID, &it.Overview, &it.PosterPath, &it.BackdropPath, &it.StillPath, &it.Rating, &it.Genres,
			&it.Season, &it.Episode, &it.AirDate, &it.SourceType, &it.AddedAt); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ---------- transient-failure retry ----------

// retryAttempts and retryBackoff bound the retry of a statement that failed for
// a reason that is not about the statement.
//
// A library scan issues thousands of writes over tens of minutes against a
// database file that a virus scanner, a backup agent or the filesystem itself
// can interfere with for a moment. SQLite reports that as "unable to open
// database file" — the same code it uses when it cannot create one of its own
// temporary files — and one such hiccup used to abort the whole scan, throwing
// away everything it had organized so far. A single retry after a short pause
// rides it out, and a genuine failure still surfaces on the last attempt.
const (
	retryAttempts = 4
	retryBackoff  = 150 * time.Millisecond
)

// retryableDBError reports errors worth trying again: the file was momentarily
// unavailable or the database was busy. Constraint violations, missing columns
// and the like are not: those mean the statement or the schema is wrong, and
// repeating them only wastes time.
func retryableDBError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{
		"unable to open database file",
		"database is locked",
		"database table is locked",
		"database is busy",
		"database or disk is full",
		"disk i/o error",
		"file is not a database",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// execRetry runs a statement, retrying the failures that are about the
// environment rather than the statement.
func (s *Store) execRetry(query string, args ...any) error {
	_, err := s.execRetryResult(query, args...)
	return err
}

// execRetryResult is execRetry for callers that need the result, such as an
// insert whose new row id has to be read back.
func (s *Store) execRetryResult(query string, args ...any) (sql.Result, error) {
	var res sql.Result
	var err error
	for i := 0; i < retryAttempts; i++ {
		if res, err = s.DB.Exec(query, args...); err == nil {
			return res, nil
		}
		if !retryableDBError(err) {
			return nil, err
		}
		time.Sleep(retryBackoff * time.Duration(i+1))
	}
	return nil, err
}

// queryItems runs a multi-row read, with a fallback for a failure observed in
// this deployment that the statement and the file cannot explain.
//
// The symptom: after a library scan's walk spends minutes listing directories,
// the organizer's next large multi-row read fails with SQLite's "unable to open
// database file" (SQLITE_CANTOPEN), and the whole organize phase is discarded.
// Everything that would normally explain that code was ruled out by measurement
// inside the failing process — the database opens, integrity-checks and accepts
// writes from a fresh process; the same file answers COUNT(*) on the very same
// connection at the very same moment; the statement, its parameters, the pool
// (1 open, 0 waiting), the temp directory and the disk are all fine; and the
// identical statement run through QueryRow on that same connection succeeds.
//
// So the fallback is the one thing measured to work: when Query fails this way,
// read the same rows through QueryRow instead. It is deliberately not a retry —
// repeating the call on the same connection fails identically, which is why
// retrying changed nothing. QueryRow taking a different path through the driver
// is the difference, and it is enough to carry the scan past a step that was
// otherwise fatal.
func (s *Store) queryItems(query string, args ...any) ([]models.Item, error) {
	rows, err := s.DB.Query(query, args...)
	if err == nil {
		defer rows.Close()
		return scanItems(rows)
	}
	if !retryableDBError(err) {
		return nil, err
	}
	log.Printf("items query failed, reading it through QueryRow instead: %v", err)
	return s.itemsViaQueryRow(query, args...)
}

// itemsViaQueryRow reads a query one row at a time through QueryRow. Same
// columns, same scanning, different driver path — which is the whole point.
func (s *Store) itemsViaQueryRow(query string, args ...any) ([]models.Item, error) {
	var out []models.Item
	for {
		it, dest := itemDestinations()
		err := s.DB.QueryRow(query, args...).Scan(dest...)
		if err == sql.ErrNoRows {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, *it)
	}
}

// itemDestinations builds one models.Item plus the scan targets that fill it, in
// the column order every items SELECT in this file uses.
func itemDestinations() (*models.Item, []any) {
	it := &models.Item{}
	dest := []any{
		&it.ID, &it.LibraryID, &it.Type, &it.ParentID, &it.Name, &it.OriginalTitle,
		&it.Year, &it.TMDBID, &it.BGMID, &it.Overview, &it.PosterPath, &it.BackdropPath,
		&it.StillPath, &it.Rating, &it.Genres, &it.Season, &it.Episode, &it.AirDate, &it.AddedAt,
	}
	return it, dest
}

// ---------- files ----------

// UpsertFile stores a file and (re)binds it to its current item: a rescan that
// changes how a path is grouped moves the row instead of duplicating it.
// An empty probe never overwrites a stored probe result.
func (s *Store) UpsertFile(f *models.MediaFile) (id int64, err error) {
	err = s.execRetry(`INSERT INTO files (item_id, source_id, path, size, mtime, probe, added_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(source_id, path) DO UPDATE SET
			item_id=excluded.item_id,
			size=excluded.size,
			mtime=excluded.mtime,
			probe=CASE WHEN excluded.probe <> '' THEN excluded.probe ELSE files.probe END`,
		f.ItemID, f.SourceID, f.Path, f.Size, f.MTime, f.Probe, time.Now())
	if err != nil {
		return 0, err
	}
	if err := s.DB.QueryRow(`SELECT id FROM files WHERE source_id=? AND path=?`, f.SourceID, f.Path).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Store) FileByID(id int64) (*models.MediaFile, error) {
	f := &models.MediaFile{}
	err := s.DB.QueryRow(`SELECT id, item_id, source_id, path, size, mtime, probe, added_at FROM files WHERE id=?`, id).
		Scan(&f.ID, &f.ItemID, &f.SourceID, &f.Path, &f.Size, &f.MTime, &f.Probe, &f.AddedAt)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// FileBySourcePath finds a file by its driver path regardless of which item
// owns it — used so the file browser plays an already-scanned file instead of
// re-binding it to a browse scratchpad item.
func (s *Store) FileBySourcePath(sourceID int64, path string) (*models.MediaFile, error) {
	f := &models.MediaFile{}
	err := s.DB.QueryRow(`SELECT id, item_id, source_id, path, size, mtime, probe, added_at
		FROM files WHERE source_id = ? AND path = ?`, sourceID, path).
		Scan(&f.ID, &f.ItemID, &f.SourceID, &f.Path, &f.Size, &f.MTime, &f.Probe, &f.AddedAt)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// FileByPath finds an existing file row (within one library) by its driver path.
func (s *Store) FileByPath(libraryID int64, path string) (*models.MediaFile, error) {
	f := &models.MediaFile{}
	err := s.DB.QueryRow(`SELECT f.id, f.item_id, f.source_id, f.path, f.size, f.mtime, f.probe, f.added_at
		FROM files f JOIN items i ON i.id = f.item_id WHERE i.library_id = ? AND f.path = ?`, libraryID, path).
		Scan(&f.ID, &f.ItemID, &f.SourceID, &f.Path, &f.Size, &f.MTime, &f.Probe, &f.AddedAt)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (s *Store) FilesByItem(itemID int64) ([]models.MediaFile, error) {
	rows, err := s.DB.Query(`SELECT id, item_id, source_id, path, size, mtime, probe, added_at FROM files WHERE item_id=? ORDER BY id`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.MediaFile
	for rows.Next() {
		var f models.MediaFile
		if err := rows.Scan(&f.ID, &f.ItemID, &f.SourceID, &f.Path, &f.Size, &f.MTime, &f.Probe, &f.AddedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FileShowIndex maps every episode file of a library to the show it currently
// belongs to, keyed by source and path. A scan uses it to recognise a file it
// has already organized: a show's name can be overwritten by a metadata
// provider between two scans (「装甲重拳 MEGALOBOX」 → 「MEGALO BOX」), and a
// scanner that matched shows by name alone then created a second show for the
// same folder on every scan. The file's own path is the stable identity.
//
// split marks a show whose files do not all sit under one source folder. Such a
// show is the product of an older, wrong grouping (bilibili's `TV [season N]`
// folders once made every single-season series one "TV" show); it must not be
// used as an anchor, or the rescan would preserve the wrong grouping instead of
// repairing it.
func (s *Store) FileShowIndex(libraryID int64) (byFile map[string]int64, split map[int64]bool, err error) {
	rows, err := s.DB.Query(`SELECT f.source_id, f.path, sh.id
		FROM files f
		JOIN items ep ON ep.id = f.item_id AND ep.type = 'episode'
		JOIN items se ON se.id = ep.parent_id AND se.type = 'season'
		JOIN items sh ON sh.id = se.parent_id AND sh.type = 'show'
		WHERE sh.library_id = ?`, libraryID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	byFile = map[string]int64{}
	roots := map[int64]map[string]bool{}
	for rows.Next() {
		var sourceID, showID int64
		var path string
		if err := rows.Scan(&sourceID, &path, &showID); err != nil {
			return nil, nil, err
		}
		byFile[fmt.Sprintf("%d|%s", sourceID, path)] = showID
		root := fmt.Sprintf("%d|%s", sourceID, firstPathSegment(path))
		if roots[showID] == nil {
			roots[showID] = map[string]bool{}
		}
		roots[showID][root] = true
	}
	split = map[int64]bool{}
	for showID, set := range roots {
		if len(set) > 1 {
			split[showID] = true
		}
	}
	return byFile, split, rows.Err()
}

func firstPathSegment(path string) string {
	p := strings.TrimPrefix(strings.TrimSpace(path), "/")
	if i := strings.Index(p, "/"); i >= 0 {
		return p[:i]
	}
	return p
}

func (s *Store) UpdateFileProbe(id int64, probe string) error {
	_, err := s.DB.Exec(`UPDATE files SET probe=? WHERE id=?`, probe, id)
	return err
}

// FilesWithoutProbe returns playable files of a library that have no cached
// probe yet, newest first. Used to warm the probe cache after a scan so the
// first play of a file does not pay the (slow) probe. limit <= 0 means no limit.
func (s *Store) FilesWithoutProbe(libraryID int64, limit int) ([]*models.MediaFile, error) {
	q := `SELECT f.id, f.item_id, f.source_id, f.path, f.size, f.mtime, f.added_at
		FROM files f JOIN items i ON i.id = f.item_id
		WHERE i.library_id = ? AND (f.probe IS NULL OR f.probe = '')
		ORDER BY f.id DESC`
	args := []any{libraryID}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*models.MediaFile
	for rows.Next() {
		f := &models.MediaFile{}
		if err := rows.Scan(&f.ID, &f.ItemID, &f.SourceID, &f.Path, &f.Size, &f.MTime, &f.AddedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) DeleteFilesNotIn(itemID int64, keep map[string]bool) error {
	rows, err := s.DB.Query(`SELECT id, path FROM files WHERE item_id=?`, itemID)
	if err != nil {
		return err
	}
	var toDelete []int64
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			rows.Close()
			return err
		}
		if !keep[p] {
			toDelete = append(toDelete, id)
		}
	}
	rows.Close()
	for _, id := range toDelete {
		if _, err := s.DB.Exec(`DELETE FROM files WHERE id=?`, id); err != nil {
			return err
		}
	}
	return nil
}

// ---------- subtitles ----------

func (s *Store) UpsertSubtitle(sub *models.Subtitle) error {
	_, err := s.DB.Exec(`INSERT INTO subtitles (file_id, path, lang, title, format, embedded, stream_idx, convertible)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(file_id, path, stream_idx) DO UPDATE SET lang=excluded.lang, title=excluded.title,
		format=excluded.format, embedded=excluded.embedded, convertible=excluded.convertible`,
		sub.FileID, sub.Path, sub.Lang, sub.Title, sub.Format, sub.Embedded, sub.StreamIdx, sub.Convertible)
	return err
}

func (s *Store) SubtitlesByFile(fileID int64) ([]models.Subtitle, error) {
	rows, err := s.DB.Query(`SELECT id, file_id, path, lang, title, format, embedded, stream_idx, convertible
		FROM subtitles WHERE file_id=? ORDER BY id`, fileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Subtitle
	for rows.Next() {
		var sub models.Subtitle
		var embedded, convertible int
		if err := rows.Scan(&sub.ID, &sub.FileID, &sub.Path, &sub.Lang, &sub.Title, &sub.Format, &embedded, &sub.StreamIdx, &convertible); err != nil {
			return nil, err
		}
		sub.Embedded = embedded == 1
		sub.Convertible = convertible == 1
		out = append(out, sub)
	}
	return out, rows.Err()
}

func (s *Store) SubtitleByID(id int64) (*models.Subtitle, error) {
	sub := &models.Subtitle{}
	var embedded, convertible int
	err := s.DB.QueryRow(`SELECT id, file_id, path, lang, title, format, embedded, stream_idx, convertible
		FROM subtitles WHERE id=?`, id).
		Scan(&sub.ID, &sub.FileID, &sub.Path, &sub.Lang, &sub.Title, &sub.Format, &embedded, &sub.StreamIdx, &convertible)
	if err != nil {
		return nil, err
	}
	sub.Embedded = embedded == 1
	sub.Convertible = convertible == 1
	return sub, nil
}

// ---------- progress ----------

func (s *Store) SaveProgress(p *models.Progress) error {
	_, err := s.DB.Exec(`INSERT INTO progress (user_id, file_id, item_id, position, duration, updated_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(user_id, file_id) DO UPDATE SET position=excluded.position, duration=excluded.duration,
		item_id=excluded.item_id, updated_at=excluded.updated_at`,
		p.UserID, p.FileID, p.ItemID, p.Position, p.Duration, time.Now())
	return err
}

func (s *Store) ProgressByUser(userID int64) ([]models.Progress, error) {
	rows, err := s.DB.Query(`SELECT user_id, file_id, item_id, position, duration, updated_at
		FROM progress WHERE user_id=? AND position > 30 ORDER BY updated_at DESC LIMIT 50`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Progress
	for rows.Next() {
		var p models.Progress
		if err := rows.Scan(&p.UserID, &p.FileID, &p.ItemID, &p.Position, &p.Duration, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) ProgressForFile(userID, fileID int64) (*models.Progress, error) {
	p := &models.Progress{}
	err := s.DB.QueryRow(`SELECT user_id, file_id, item_id, position, duration, updated_at
		FROM progress WHERE user_id=? AND file_id=?`, userID, fileID).
		Scan(&p.UserID, &p.FileID, &p.ItemID, &p.Position, &p.Duration, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return p, nil
}
