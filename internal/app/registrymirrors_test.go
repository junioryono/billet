package app

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"go/ast"
	"go/parser"
	"go/token"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// aCertificatePEM is one self-signed certificate, an authority or not.
func aCertificatePEM(t *testing.T, authority bool) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "billet test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: authority, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create a certificate: %v", err)
	}

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// THE SECRET A REFUSAL OR AN ANSWER MUST NEVER CARRY. It is valid base64, so a
// damaged block holding it is one pem.Decode reads past.
const mirrorSecret = "c2VjcmV0"

func TestARegistryMirrorAuthorityReachesAGuestOnlyAsCertificates(t *testing.T) {
	t.Parallel()

	authority := aCertificatePEM(t, true)
	second := aCertificatePEM(t, true)
	const (
		key        = "-----BEGIN PRIVATE KEY-----\n" + mirrorSecret + "\n-----END PRIVATE KEY-----\n"
		damagedKey = "-----BEGIN PRIVATE KEY-----\n" + mirrorSecret + "\n-----END RSA PRIVATE KEY-----\n"
		damagedPEM = "-----BEGIN CERTIFICATE-----\n" + mirrorSecret + "\n-----END PRIVATE KEY-----\n"
	)
	withHeader := strings.Replace(authority, "-----BEGIN CERTIFICATE-----\n",
		"-----BEGIN CERTIFICATE-----\nNote: "+mirrorSecret+"\n\n", 1)

	const refused = "not a PEM certificate"
	for _, tc := range []struct {
		name, body, want, refusal string
	}{
		{name: "an authority", body: authority, want: authority},
		{name: "two authorities", body: authority + "\n" + second, want: authority + second},
		{name: "surrounding blank lines", body: "\n\n" + authority + "\n\n", want: authority},
		// A DAMAGED CERTIFICATE AHEAD OF A GOOD ONE is read past by the decoder,
		// and all that matters then is that its bytes reach nobody.
		{name: "a damaged certificate before one", body: damagedPEM + authority, want: authority},

		{name: "a private key after it", body: authority + key, refusal: refused},
		{name: "a private key", body: key, refusal: refused},
		{name: "a private key before it", body: key + authority, refusal: refused},
		{name: "a damaged private key before it", body: damagedKey + authority, refusal: refused},
		{name: "a damaged private key after it", body: authority + damagedKey, refusal: refused},
		{name: "text before it", body: mirrorSecret + "\n" + authority, refusal: refused},
		{name: "text between two", body: authority + mirrorSecret + "\n" + second, refusal: refused},
		{name: "text after it", body: authority + "and more\n", refusal: refused},
		{name: "a header on it", body: withHeader, refusal: refused},
		{name: "a certificate that is no authority", body: aCertificatePEM(t, false), refusal: "no authority"},
		{name: "nothing", body: "\n", refusal: "no certificate"},
		{
			name: "a block that does not parse", refusal: "does not parse",
			body: "-----BEGIN CERTIFICATE-----\n" + mirrorSecret + "\n-----END CERTIFICATE-----\n",
		},
		{
			name: "more than a guest's metadata should carry", refusal: "larger than the limit",
			body: authority + strings.Repeat(" ", registryMirrorsCALimit),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "ca.pem")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatalf("write the file: %v", err)
			}
			got, err := ReadRegistryMirrorsCA(path)
			if strings.Contains(got, mirrorSecret) {
				t.Fatalf("the answer carries what was not a certificate: %q", got)
			}
			if tc.refusal == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if got != tc.want {
					t.Fatalf("read %q, want %q", got, tc.want)
				}

				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.refusal) {
				t.Fatalf("error = %v, want one saying %q", err, tc.refusal)
			}
			if got != "" {
				t.Fatalf("a refusal still answered %q", got)
			}
			// THE REFUSAL NAMES THE FILE, never what is in it.
			if strings.Contains(err.Error(), mirrorSecret) {
				t.Fatalf("the refusal quoted the file: %v", err)
			}
		})
	}
}

func TestAnUnreadableRegistryMirrorAuthorityIsARefusal(t *testing.T) {
	t.Parallel()

	if _, err := ReadRegistryMirrorsCA(filepath.Join(t.TempDir(), "absent.pem")); err == nil {
		t.Fatal("an absent authority answered nothing wrong")
	}
}

// A NAMED AUTHORITY THAT IS NOT THERE IS A REFUSAL, unlike the role's own path,
// which is absent on every host the role does not run mirrors on.
func TestANamedRegistryMirrorAuthorityMustBeThere(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	if _, err := NodeRegistryMirrorsCA(&config.NodeConfig{
		RegistryMirrorsCA: filepath.Join(directory, "absent.pem"),
	}); err == nil {
		t.Error("an authority the configuration names and the host lacks was not refused")
	}

	authority := aCertificatePEM(t, true)
	path := filepath.Join(directory, "ca.pem")
	if err := os.WriteFile(path, []byte(authority), 0o600); err != nil {
		t.Fatalf("write the file: %v", err)
	}
	got, err := NodeRegistryMirrorsCA(&config.NodeConfig{RegistryMirrorsCA: path})
	if err != nil || got != authority {
		t.Errorf("a named authority = %q, %v", got, err)
	}
}

// THE ROLE WRITES THE PATH THE NODE READS, and neither reads the other, so this is
// what keeps the two one path.
func TestTheRoleInstallsTheAuthorityWhereTheNodeLooks(t *testing.T) {
	t.Parallel()

	vars, err := os.ReadFile(filepath.Join("..", "..", "ansible_collections", "junioryono", "billet",
		"roles", "host", "vars", "main.yml"))
	if err != nil {
		t.Fatalf("read the role's vars: %v", err)
	}
	if want := "billet_registry_mirror_dir: " + filepath.Dir(config.DefaultRegistryMirrorsCA) + "\n"; !strings.Contains(string(vars), want) {
		t.Errorf("the role's vars do not say %q", want)
	}
	if got := filepath.Base(config.DefaultRegistryMirrorsCA); got != "ca.pem" {
		t.Errorf("the node looks for %s, and the role writes ca.pem", got)
	}
}

// THE ROLE'S AUTHORITY REACHES A GUEST ONLY IF THE NODE'S ASSEMBLY PASSES IT, which
// nothing else here can see: the runner's own test hands it a value directly.
func TestTheNodesAssemblyGivesTheRunnerTheAuthority(t *testing.T) {
	t.Parallel()

	file, err := parser.ParseFile(token.NewFileSet(), "node.go", nil, 0)
	if err != nil {
		t.Fatalf("parse node.go: %v", err)
	}

	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "WithRegistryMirrors" || len(call.Args) != 2 {
			return true
		}
		lit, ok := call.Args[1].(*ast.FuncLit)
		if !ok {
			t.Errorf("the second argument of WithRegistryMirrors is %T, want the read at each launch", call.Args[1])

			return true
		}
		ast.Inspect(lit, func(inner ast.Node) bool {
			if read, ok := inner.(*ast.CallExpr); ok {
				if id, ok := read.Fun.(*ast.Ident); ok && id.Name == "NodeRegistryMirrorsCA" {
					found = true
				}
			}

			return true
		})

		return true
	})
	if !found {
		t.Error("node.go does not give the runner NodeRegistryMirrorsCA, so no guest is handed the authority")
	}
}
