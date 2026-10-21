package state

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	_ fmt.Stringer   = ClientEnv{}
	_ fmt.GoStringer = ClientEnv{}
	_ fmt.Formatter  = ClientEnv{}
	_ json.Marshaler = ClientEnv{}
	_ slog.LogValuer = ClientEnv{}
)

// envOf renders a DSN and returns its variables as a map, failing the test on a
// refusal.
func envOf(t *testing.T, dsn string) map[string]string {
	t.Helper()

	env, err := LibpqEnv(DSN(dsn))
	if err != nil {
		t.Fatalf("LibpqEnv: %v", err)
	}

	out := map[string]string{}

	for _, v := range env.Environ(nil) {
		name, value, _ := strings.Cut(v, "=")
		out[name] = value
	}

	return out
}

// THE DSN SHAPES BILLET'S OWN SUITES AND OPERATORS USE, rendered as libpq reads
// them. The search_path one is the reason this exists at all: libpq refuses it
// as a URI parameter (measured 2026-10-20, "invalid URI query parameter"), and a
// dump taken without it would be of whatever schema the role defaults to.
func TestADSNIsRenderedAsTheEnvironmentLibpqReads(t *testing.T) {
	t.Parallel()

	// pgx LOADS sslrootcert WHILE IT PARSES, so the file has to be a real
	// certificate for the cross-check to run at all.
	rootCert := writeTestCA(t)

	cases := []struct {
		name string
		dsn  string
		want map[string]string
	}{
		{
			name: "a URI with a pgx runtime parameter",
			dsn:  "postgres://billet:" + testDSNPassword + "@127.0.0.1:5432/billet?sslmode=disable&search_path=billet_test_x",
			want: map[string]string{
				"PGHOST": "127.0.0.1", "PGPORT": "5432", "PGUSER": "billet",
				"PGPASSWORD": testDSNPassword, "PGDATABASE": "billet", "PGSSLMODE": "disable",
				"PGOPTIONS": "-c search_path=billet_test_x",
			},
		},
		{
			name: "keywords with a quoted value and TLS file paths",
			dsn: "host=db.internal port=6543 user=billet password='with space " + testDSNPassword + "' " +
				"dbname=ledger sslmode=verify-full sslrootcert=" + rootCert,
			want: map[string]string{
				"PGHOST": "db.internal", "PGPORT": "6543", "PGUSER": "billet",
				"PGPASSWORD": "with space " + testDSNPassword,
				"PGDATABASE": "ledger", "PGSSLMODE": "verify-full",
				"PGSSLROOTCERT": rootCert,
			},
		},
		{
			name: "a backslash-escaped quote, as libpq writes one",
			dsn:  `host=h user=u password='a\'b' dbname=d sslmode=disable`,
			want: map[string]string{"PGPASSWORD": "a'b", "PGDATABASE": "d"},
		},
		{
			name: "a runtime parameter with a space is escaped for PGOPTIONS",
			dsn:  "postgres://u@h/d?sslmode=disable&application_name=billet&search_path=a%20b",
			want: map[string]string{"PGAPPNAME": "billet", "PGOPTIONS": `-c search_path=a\ b`},
		},
		{
			name: "pgxpool's settings mean nothing to libpq and are left out",
			dsn:  "postgres://u@h/d?sslmode=disable&pool_max_conns=4&statement_cache_capacity=0",
			want: map[string]string{"PGHOST": "h", "PGDATABASE": "d"},
		},
		{
			name: "what the text leaves to defaults comes from pgx's own resolution",
			dsn:  "postgres://u@h/d?sslmode=disable",
			want: map[string]string{"PGPORT": "5432", "PGUSER": "u"},
		},
		{
			name: "several hosts, each with its own port",
			dsn:  "postgres://u@h1:5433,h2:5434/d?sslmode=disable",
			want: map[string]string{"PGHOST": "h1,h2", "PGPORT": "5433,5434"},
		},
		{
			name: "several hosts as keywords",
			dsn:  "host=h1,h2 port=5433,5434 user=u dbname=d sslmode=disable",
			want: map[string]string{"PGHOST": "h1,h2", "PGPORT": "5433,5434"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := envOf(t, tc.dsn)

			for name, want := range tc.want {
				if got[name] != want {
					t.Errorf("%s = %q, want %q (all: %v)", name, got[name], want, keys(got))
				}
			}

			if strings.Contains(got["PGOPTIONS"], "pool_") || strings.Contains(got["PGOPTIONS"], "cache_capacity") {
				t.Errorf("PGOPTIONS carries a pgx setting: %q", got["PGOPTIONS"])
			}

			if _, ok := tc.want["PGOPTIONS"]; !ok {
				if _, set := got["PGOPTIONS"]; set {
					t.Errorf("PGOPTIONS = %q, want none: the DSN sets no runtime parameter", got["PGOPTIONS"])
				}
			}
		})
	}
}

// THE PASSWORD TRAVELS IN PGPASSWORD AND NOWHERE ELSE: not in PGOPTIONS (where
// an unrecognised keyword would put it), not in any other variable, and never in
// what the type renders.
func TestThePasswordIsOnlyInPGPASSWORD(t *testing.T) {
	t.Parallel()

	got := envOf(t, "postgres://billet:"+testDSNPassword+"@h/d?sslmode=disable&search_path=s")

	for name, value := range got {
		if name != "PGPASSWORD" && strings.Contains(value, testDSNPassword) {
			t.Errorf("%s carries the password: %q", name, value)
		}
	}

	if got["PGPASSWORD"] != testDSNPassword {
		t.Fatalf("PGPASSWORD = %q, want the DSN's password", got["PGPASSWORD"])
	}
}

// A KEYWORD LIBPQ CANNOT BE HANDED IS REFUSED, NEVER DROPPED OR GUESSED. The
// one that matters is sslpassword: carried as a runtime parameter it would put
// the key's passphrase into PGOPTIONS, and dropped it would leave pg_dump unable
// to read the key billet's own connection reads. The refusal names the keyword
// and quotes nothing of the DSN.
func TestAKeywordLibpqCannotReadIsRefused(t *testing.T) {
	t.Parallel()

	for _, keyword := range []string{"sslpassword", "krbspn", "service", "servicefile", "hostaddr"} {
		dsn := "host=h user=u dbname=d sslmode=disable " + keyword + "=" + testDSNPassword

		_, err := LibpqEnv(DSN(dsn))
		if err == nil {
			t.Fatalf("%s was accepted", keyword)
		}

		if !strings.Contains(err.Error(), keyword) {
			t.Errorf("the refusal does not name %s: %v", keyword, err)
		}

		if strings.Contains(err.Error(), testDSNPassword) {
			t.Errorf("the refusal quotes the DSN: %v", err)
		}
	}
}

// AN UNPARSABLE DSN IS errUnparsableDSN, which quotes nothing of it.
func TestAnUnparsableDSNIsRefusedWithoutQuotingIt(t *testing.T) {
	t.Parallel()

	for _, dsn := range []string{
		"postgres://billet:" + testDSNPassword + "@h:notaport/d",
		"host=h password='" + testDSNPassword,
		"",
	} {
		_, err := LibpqEnv(DSN(dsn))
		if err == nil {
			t.Fatalf("a DSN of %d bytes was accepted", len(dsn))
		}

		if dsn != "" && !errors.Is(err, errUnparsableDSN) {
			t.Errorf("err = %v, want errUnparsableDSN", err)
		}

		if strings.Contains(err.Error(), testDSNPassword) {
			t.Errorf("the refusal quotes the DSN: %v", err)
		}
	}
}

// THE PARENT'S PG* VARIABLES ARE KEPT WHERE THE DSN IS SILENT AND REPLACED
// WHERE IT SPEAKS, which is how pgx read them for billet's own connection.
func TestEnvironLaysTheConnectionOverTheParent(t *testing.T) {
	t.Parallel()

	env, err := LibpqEnv("postgres://u:p@h/d?search_path=s")
	if err != nil {
		t.Fatalf("LibpqEnv: %v", err)
	}

	got := env.Environ([]string{
		"PATH=/usr/bin", "PGHOST=elsewhere", "PGSSLMODE=require", "PGHOSTADDR=10.0.0.9",
	})

	want := map[string]bool{"PATH=/usr/bin": true, "PGHOST=h": true, "PGSSLMODE=require": true}
	for w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("Environ lacks %s: %v", w, got)
		}
	}

	if slices.Contains(got, "PGHOST=elsewhere") {
		t.Errorf("the parent's PGHOST survived the DSN's: %v", got)
	}

	// ONE pgx NEVER READS IS DROPPED: libpq would dial PGHOSTADDR instead of the
	// host billet's own connection reached.
	for _, v := range got {
		if strings.HasPrefix(v, "PGHOSTADDR=") {
			t.Errorf("the parent's PGHOSTADDR reached the client program: %v", got)
		}
	}
}

// THE ENDPOINTS ARE THE ONES pgx DIALS, whatever the text's shape: a host list
// whose ports libpq and pgx would map differently renders as pgx's resolution,
// primary first, so pg_dump reaches the server billet's own connection reaches.
func TestTheEndpointsAreTheOnesPgxDials(t *testing.T) {
	t.Parallel()

	for _, dsn := range []string{
		"postgres://u@h1,h2:6543/d?sslmode=disable",
		"host=h1,h2 port=6543 user=u dbname=d sslmode=disable",
	} {
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil {
			t.Fatalf("pgx.ParseConfig(%q): %v", dsn, err)
		}

		var hosts, ports []string

		endpoints := append([]*pgconn.FallbackConfig{{Host: cfg.Host, Port: cfg.Port}}, cfg.Fallbacks...)
		for _, ep := range endpoints {
			port := strconv.Itoa(int(ep.Port))

			seen := false

			for i := range hosts {
				if hosts[i] == ep.Host && ports[i] == port {
					seen = true
				}
			}

			if !seen {
				hosts, ports = append(hosts, ep.Host), append(ports, port)
			}
		}

		got := envOf(t, dsn)

		if got["PGHOST"] != strings.Join(hosts, ",") || got["PGPORT"] != strings.Join(ports, ",") {
			t.Errorf("%q renders as PGHOST=%q PGPORT=%q, want pgx's %q and %q",
				dsn, got["PGHOST"], got["PGPORT"], strings.Join(hosts, ","), strings.Join(ports, ","))
		}
	}
}

// A SETTING IN THIS PROCESS'S ENVIRONMENT THAT libpq WOULD TAKE DIFFERENTLY IS
// REFUSED BY NAME: a service file overrides libpq's environment where pgx lets
// the connection string override it, and libpq has no sslpassword variable.
func TestAnInheritedSettingLibpqTakesDifferentlyIsRefused(t *testing.T) {
	for _, name := range []string{"PGSERVICE", "PGSERVICEFILE", "PGSSLPASSWORD"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, testDSNPassword)

			_, err := LibpqEnv("postgres://u@h/d?sslmode=disable")
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("LibpqEnv with %s set = %v, want a refusal naming it", name, err)
			}

			if strings.Contains(err.Error(), testDSNPassword) {
				t.Errorf("the refusal quotes the variable's value: %v", err)
			}
		})
	}
}

// Every rendering path redacts, and Redact removes the password from text a
// client program wrote.
func TestAClientEnvIsRedactedOnEveryRenderingPath(t *testing.T) {
	t.Parallel()

	env, err := LibpqEnv(DSN("postgres://billet:" + testDSNPassword + "@h/d?sslmode=disable"))
	if err != nil {
		t.Fatalf("LibpqEnv: %v", err)
	}

	holder := struct{ Env ClientEnv }{env}

	encoded, err := json.Marshal(holder)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var logged bytes.Buffer

	slog.New(slog.NewJSONHandler(&logged, nil)).Info("dumping", "env", env)

	for name, rendered := range map[string]string{
		"%v":             fmt.Sprintf("%v", env), //nolint:gocritic // the verb path is the subject
		"%+v on a field": fmt.Sprintf("%+v", holder),
		"%#v":            fmt.Sprintf("%#v", env),
		"%x":             fmt.Sprintf("%x", env),
		"json":           string(encoded),
		"slog json":      logged.String(),
		"Redact":         env.Redact("FATAL: password authentication failed: " + testDSNPassword),
	} {
		if strings.Contains(rendered, testDSNPassword) {
			t.Errorf("%s leaks the password: %s", name, rendered)
		}
	}
}

// writeTestCA writes a self-signed certificate and returns its path.
func writeTestCA(t *testing.T) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "billet test database CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create a certificate: %v", err)
	}

	path := filepath.Join(t.TempDir(), "db-ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write the certificate: %v", err)
	}

	return path
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	slices.Sort(out)

	return out
}

// THE ENDPOINTS, PINNED: values read off pgx's resolution once and written
// down, so a rendering that took the text's ports, or that a change to the
// dedup broke, fails here and not only in a test computing its answer the way
// production does. The default sslmode=prefer is the case the dedup exists
// for: pgx keeps a TLS and a plain attempt per host.
func TestTheEndpointsArePinned(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ dsn, host, port string }{
		{"postgres://u@h1,h2:6543/d?sslmode=disable", "h1,h2", "6543,6543"},
		{"postgres://u@h1,h2:6543/d", "h1,h2", "6543,6543"},
		{"postgres://u@h1:5433,h2:5434/d", "h1,h2", "5433,5434"},
		{"host=h1,h2 port=6543 user=u dbname=d", "h1,h2", "6543,6543"},
	} {
		got := envOf(t, tc.dsn)

		if got["PGHOST"] != tc.host || got["PGPORT"] != tc.port {
			t.Errorf("%q renders as PGHOST=%q PGPORT=%q, want %q and %q",
				tc.dsn, got["PGHOST"], got["PGPORT"], tc.host, tc.port)
		}
	}
}

// AN EMPTY HOST IS REFUSED, or rendered as a host: libpq reads an empty one as
// its default socket, which is not where pgx goes.
func TestAnEmptyHostIsNeverRendered(t *testing.T) {
	t.Parallel()

	for _, dsn := range []string{
		"host=,db.internal port=5432 user=u dbname=d sslmode=disable",
		"host=db.internal, port=5432 user=u dbname=d sslmode=disable",
	} {
		env, err := LibpqEnv(DSN(dsn))
		if err != nil {
			continue
		}

		for _, v := range env.Environ(nil) {
			if host, ok := strings.CutPrefix(v, "PGHOST="); ok && slices.Contains(strings.Split(host, ","), "") {
				t.Errorf("%q renders an empty host: %s", dsn, v)
			}
		}
	}
}

// A VERTICAL TAB OR A FORM FEED ENDS A VALUE FOR BOTH READINGS, as pgx's
// asciiSpace does, so a keyword behind one is seen and refused here rather
// than acted on by pgx alone.
func TestAKeywordBehindAnUnusualSpaceIsStillSeen(t *testing.T) {
	t.Parallel()

	for _, sep := range []string{"\v", "\f"} {
		dsn := "host=h" + sep + "service=billet dbname=d user=u"

		_, err := LibpqEnv(DSN(dsn))
		if err == nil || !strings.Contains(err.Error(), "service") {
			t.Errorf("a service keyword behind %q = %v, want a refusal naming it", sep, err)
		}
	}
}

// WHAT THE CHILD INHERITS IS RENDERED AS pgx READ IT: an inherited PGOPTIONS
// stays beside the DSN's runtime parameters, and an inherited password is
// redacted like the DSN's own.
func TestWhatTheChildInheritsIsRenderedAsPgxReadIt(t *testing.T) {
	t.Setenv("PGOPTIONS", "-c role=ledger_owner")
	t.Setenv("PGPASSWORD", testDSNPassword)

	env, err := LibpqEnv("postgres://u@h/d?sslmode=disable&search_path=billet")
	if err != nil {
		t.Fatalf("LibpqEnv: %v", err)
	}

	var options string

	for _, v := range env.Environ([]string{"PGOPTIONS=-c role=ledger_owner", "PGPASSWORD=" + testDSNPassword}) {
		if o, ok := strings.CutPrefix(v, "PGOPTIONS="); ok {
			options = o
		}
	}

	if options != "-c role=ledger_owner -c search_path=billet" {
		t.Errorf("PGOPTIONS = %q, want the inherited options and then the DSN's", options)
	}

	if got := env.Redact("FATAL: " + testDSNPassword); strings.Contains(got, testDSNPassword) {
		t.Errorf("an inherited password survived redaction: %q", got)
	}
}
