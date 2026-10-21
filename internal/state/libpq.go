package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ClientEnv is a ledger's connection rendered as the environment the PostgreSQL
// client programs (pg_dump, pg_restore) read it from.
//
// THE ENVIRONMENT, NEVER ARGV. A connection string carries a password, and a
// program's argument vector is readable by every account on the host through
// /proc/<pid>/cmdline; its environment is readable by the same account and root.
// That is the rule a connector token already follows.
//
// AND NOT THE DSN HANDED OVER WHOLE, which is what neither half of libpq allows.
// A URI in PGDATABASE is taken as a database name rather than expanded, and
// billet's own DSNs carry pgx runtime parameters (`?search_path=` is how every
// PostgreSQL test of this package names its schema) that libpq refuses as an
// invalid URI query parameter (both measured 2026-10-20, libpq 18.1). So the
// keywords are read out of the text and each is given the variable libpq reads
// it from, a runtime parameter becoming `-c key=value` in PGOPTIONS, which is how
// pgx sends it.
//
// It redacts itself on every rendering path, because it holds the password.
type ClientEnv struct {
	vars     []string
	password string
}

// Environ returns parent with this connection's variables laid over it.
//
// THE PARENT'S PG* VARIABLES ARE KEPT where the DSN does not name the setting,
// because pgx read them too: a PGSSLMODE in the service's environment is the same
// default to billet's own connection and to the client program's.
func (e ClientEnv) Environ(parent []string) []string {
	set := make(map[string]bool, len(e.vars))
	for _, v := range e.vars {
		set[v[:strings.IndexByte(v, '=')]] = true
	}

	out := make([]string, 0, len(parent)+len(e.vars))

	for _, v := range parent {
		name, _, _ := strings.Cut(v, "=")
		if !set[name] {
			out = append(out, v)
		}
	}

	return append(out, e.vars...)
}

// Redact removes the password from text a client program wrote, before the text
// can reach an error. libpq does not print a password it was given, and this is
// the second line rather than the first: a message is the most likely thing to
// be pasted into an issue.
func (e ClientEnv) Redact(text string) string {
	if e.password == "" {
		return text
	}

	return strings.ReplaceAll(text, e.password, redactedDSN)
}

// String names the variables and never their values.
func (e ClientEnv) String() string {
	names := make([]string, 0, len(e.vars))
	for _, v := range e.vars {
		names = append(names, v[:strings.IndexByte(v, '=')])
	}

	return "state.ClientEnv{" + strings.Join(names, " ") + " " + redactedDSN + "}"
}

// GoString covers %#v, which does not consult String.
func (e ClientEnv) GoString() string { return e.String() }

// Format makes every verb safe, not only the ones fmt.Stringer covers.
func (e ClientEnv) Format(s fmt.State, verb rune) {
	//nolint:errcheck // fmt.State has no error channel; a failed write to it is the caller's output problem.
	io.WriteString(s, e.String())

	_ = verb
}

// MarshalJSON keeps the password out of anything that serializes it.
func (e ClientEnv) MarshalJSON() ([]byte, error) {
	out, err := json.Marshal(e.String())
	if err != nil {
		return nil, fmt.Errorf("state: marshal redacted client environment: %w", err)
	}

	return out, nil
}

// LogValue is what slog asks for before falling back to reflection.
func (e ClientEnv) LogValue() slog.Value { return slog.StringValue(e.String()) }

// libpqVariables is every connection keyword billet hands a client program, and
// the variable libpq reads it from.
//
// A CLOSED SET. A keyword outside it and outside pgxOnlyKeywords is refused
// rather than dropped: dropping sslrootcert would hand the client program a
// connection that verifies less than billet's own, and nothing would say so.
var libpqVariables = map[string]string{
	"host":                     "PGHOST",
	"hostaddr":                 "PGHOSTADDR",
	"port":                     "PGPORT",
	"dbname":                   "PGDATABASE",
	"database":                 "PGDATABASE",
	"user":                     "PGUSER",
	"password":                 "PGPASSWORD",
	"passfile":                 "PGPASSFILE",
	"service":                  "PGSERVICE",
	"servicefile":              "PGSERVICEFILE",
	"application_name":         "PGAPPNAME",
	"connect_timeout":          "PGCONNECT_TIMEOUT",
	"client_encoding":          "PGCLIENTENCODING",
	"sslmode":                  "PGSSLMODE",
	"sslcert":                  "PGSSLCERT",
	"sslkey":                   "PGSSLKEY",
	"sslrootcert":              "PGSSLROOTCERT",
	"sslcrl":                   "PGSSLCRL",
	"sslsni":                   "PGSSLSNI",
	"sslnegotiation":           "PGSSLNEGOTIATION",
	"requirepeer":              "PGREQUIREPEER",
	"gssencmode":               "PGGSSENCMODE",
	"krbsrvname":               "PGKRBSRVNAME",
	"channel_binding":          "PGCHANNELBINDING",
	"require_auth":             "PGREQUIREAUTH",
	"target_session_attrs":     "PGTARGETSESSIONATTRS",
	"load_balance_hosts":       "PGLOADBALANCEHOSTS",
	"ssl_min_protocol_version": "PGSSLMINPROTOCOLVERSION",
	"ssl_max_protocol_version": "PGSSLMAXPROTOCOLVERSION",
}

// pgxOnlyKeywords are settings pgx consumes itself and never sends to the
// server, so they mean nothing to a client program. A pool_ prefix is pgxpool's
// and is ignored the same way.
var pgxOnlyKeywords = map[string]bool{
	"statement_cache_capacity":   true,
	"description_cache_capacity": true,
	"default_query_exec_mode":    true,
	"min_read_buffer_size":       true,
	"fallback_application_name":  true,
}

// unrenderableKeywords change how the connection authenticates and have no
// libpq environment variable, so a client program would connect differently
// from billet. Refused by name, so the refusal can say which.
var unrenderableKeywords = map[string]bool{
	"sslpassword": true,
	"krbspn":      true,
}

// LibpqEnv renders a DSN as the environment libpq reads it from.
//
// THE TEXT IS READ HERE, BESIDE registerConn, because this package is the one
// place a connection string is read as text. pgconn.Config is not enough on its
// own: it keeps no TLS file paths (sslrootcert, sslcert and sslkey become a
// loaded tls.Config), so a rendering built from it alone would verify less than
// billet's connection does. So the keywords come from the text, and pgx's parse
// of the same text supplies the defaults pgx resolved (the host it would dial,
// the user, the database) and is the cross-check: where both name a field they
// must agree, or the client program would reach a different database.
//
// A refusal quotes nothing the DSN contains, by the rule errUnparsableDSN states.
func LibpqEnv(dsn DSN) (ClientEnv, error) {
	text := strings.TrimSpace(string(dsn))
	if text == "" {
		return ClientEnv{}, errors.New(
			"state: the PostgreSQL data source is empty; it is read from the environment " +
				"variable named by server.state.postgres.dsn_env")
	}

	cfg, err := pgx.ParseConfig(text)
	if err != nil {
		return ClientEnv{}, errUnparsableDSN
	}

	var settings map[string]string

	if strings.HasPrefix(text, "postgres://") || strings.HasPrefix(text, "postgresql://") {
		settings, err = uriSettings(text)
	} else {
		settings, err = keywordSettings(text)
	}

	if err != nil {
		return ClientEnv{}, err
	}

	vars := map[string]string{}

	var options []string

	keys := make([]string, 0, len(settings))
	for k := range settings {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	for _, key := range keys {
		value := settings[key]

		switch name, ok := libpqVariables[key]; {
		case key == "options":
			options = append([]string{value}, options...)
		case ok:
			vars[name] = value
		case pgxOnlyKeywords[key] || strings.HasPrefix(key, "pool_"):
		case unrenderableKeywords[key]:
			return ClientEnv{}, fmt.Errorf(
				"state: the PostgreSQL data source sets %s, which libpq reads from no "+
					"environment variable, so pg_dump and pg_restore would connect differently "+
					"from billet; move that setting to a service file or drop it", key)
		default:
			options = append(options, "-c "+escapeOption(key)+"="+escapeOption(value))
		}
	}

	if len(options) > 0 {
		vars["PGOPTIONS"] = strings.Join(options, " ")
	}

	if err := agreeWithPgx(vars, cfg); err != nil {
		return ClientEnv{}, err
	}

	out := make([]string, 0, len(vars))
	for name, value := range vars {
		out = append(out, name+"="+value)
	}

	sort.Strings(out)

	return ClientEnv{vars: out, password: vars["PGPASSWORD"]}, nil
}

// agreeWithPgx fills what the text left to defaults from pgx's own resolution,
// and refuses a field the two read differently.
//
// FILLED, so that a default is resolved once, by the parser billet's own
// connection used: pgx and libpq choose a default socket directory differently,
// and a dump of whatever answers at the other one is not this ledger's.
func agreeWithPgx(vars map[string]string, cfg *pgx.ConnConfig) error {
	fields := []struct{ name, pgx string }{
		{"PGHOST", cfg.Host},
		{"PGPORT", strconv.Itoa(int(cfg.Port))},
		{"PGUSER", cfg.User},
		{"PGDATABASE", cfg.Database},
	}

	multiHost := strings.Contains(vars["PGHOST"], ",")

	for _, f := range fields {
		have, ok := vars[f.name]

		switch {
		case !ok:
			if f.pgx != "" && f.pgx != "0" {
				vars[f.name] = f.pgx
			}
		case multiHost && (f.name == "PGHOST" || f.name == "PGPORT"):
			// A host list is libpq's to walk; pgx keeps its first entry in Host and
			// the rest as fallbacks, so the two are not the same shape to compare.
		case have != f.pgx:
			return fmt.Errorf("state: billet and libpq read the PostgreSQL data source's %s "+
				"differently, so pg_dump and pg_restore would reach a different database "+
				"from billet's own connection", strings.ToLower(strings.TrimPrefix(f.name, "PG")))
		}
	}

	if have, ok := vars["PGPASSWORD"]; ok && have != cfg.Password {
		return errors.New("state: billet and libpq read the PostgreSQL data source's " +
			"password differently")
	}

	return nil
}

// uriSettings reads a postgres:// URI into libpq keywords: userinfo, a host
// list, the database from the path, and every query parameter as itself.
func uriSettings(text string) (map[string]string, error) {
	u, err := url.Parse(text)
	if err != nil {
		return nil, errUnparsableDSN
	}

	out := map[string]string{}

	if u.User != nil {
		if name := u.User.Username(); name != "" {
			out["user"] = name
		}

		if password, ok := u.User.Password(); ok {
			out["password"] = password
		}
	}

	var hosts, ports []string

	for _, part := range strings.Split(u.Host, ",") {
		if part == "" {
			continue
		}

		host, port, err := net.SplitHostPort(part)
		if err != nil {
			// No port in this entry, which is ordinary; brackets are kept only
			// around an IPv6 literal that had a port.
			host, port = strings.Trim(part, "[]"), ""
		}

		hosts = append(hosts, host)
		ports = append(ports, port)
	}

	if len(hosts) > 0 {
		out["host"] = strings.Join(hosts, ",")
	}

	if strings.Join(ports, "") != "" {
		out["port"] = strings.Join(ports, ",")
	}

	if db := strings.TrimPrefix(u.Path, "/"); db != "" {
		out["dbname"] = db
	}

	for key, values := range u.Query() {
		if len(values) == 0 {
			continue
		}

		// The query wins over userinfo and path, as it does for pgx.
		out[canonicalKeyword(key)] = values[0]
	}

	return out, nil
}

// keywordSettings reads a `key=value key='quoted value'` connection string into
// libpq keywords, with libpq's quoting: a value may be single-quoted, and a
// backslash escapes the next character inside or outside quotes.
func keywordSettings(text string) (map[string]string, error) {
	out := map[string]string{}

	for s := strings.TrimSpace(text); s != ""; s = strings.TrimSpace(s) {
		eq := strings.IndexByte(s, '=')
		if eq <= 0 {
			return nil, errUnparsableDSN
		}

		key := strings.TrimSpace(s[:eq])
		s = strings.TrimLeft(s[eq+1:], " \t\n\r")

		var (
			value strings.Builder
			i     int
		)

		if strings.HasPrefix(s, "'") {
			closed := false

			for i = 1; i < len(s); i++ {
				switch c := s[i]; {
				case c == '\\' && i+1 < len(s):
					i++
					value.WriteByte(s[i])
				case c == '\'':
					closed = true
				default:
					value.WriteByte(c)
				}

				if closed {
					i++

					break
				}
			}

			if !closed {
				return nil, errUnparsableDSN
			}
		} else {
			for i = 0; i < len(s) && !isSpace(s[i]); i++ {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}

				value.WriteByte(s[i])
			}
		}

		if key == "" || strings.ContainsAny(key, " \t") {
			return nil, errUnparsableDSN
		}

		out[canonicalKeyword(key)] = value.String()
		s = s[i:]
	}

	return out, nil
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// canonicalKeyword folds pgx's spelling of the database onto libpq's.
func canonicalKeyword(key string) string {
	if key == "database" {
		return "dbname"
	}

	return key
}

// escapeOption escapes a value for PGOPTIONS, which the server splits on
// whitespace with a backslash escaping the next character.
func escapeOption(s string) string {
	var b strings.Builder

	for i := range len(s) {
		if s[i] == '\\' || isSpace(s[i]) {
			b.WriteByte('\\')
		}

		b.WriteByte(s[i])
	}

	return b.String()
}
