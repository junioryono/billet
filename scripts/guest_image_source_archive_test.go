package scripts_test

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
)

// sourceServer stands in for Launchpad, the PostgreSQL archive, Adoptium's
// releases and kernel.org at once: it serves each file at its path, answers 404
// for anything else, and records what was asked for.
type sourceServer struct {
	*httptest.Server

	mu    sync.Mutex
	files map[string]string
	asked []string
}

func newSourceServer(t *testing.T) *sourceServer {
	t.Helper()

	s := &sourceServer{files: map[string]string{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.asked = append(s.asked, r.URL.Path)
		body, ok := s.files[r.URL.Path]
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)

			return
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(s.Close)

	return s
}

func digest(body string) string {
	sum := sha256.Sum256([]byte(body))

	return hex.EncodeToString(sum[:])
}

// debianSource serves a source package at DIR: a .dsc naming FILES (name to
// body) under Checksums-Sha256, and each file.
func (s *sourceServer) debianSource(dir, dsc string, files map[string]string) {
	var b strings.Builder
	b.WriteString("Format: 3.0 (quilt)\nChecksums-Sha256:\n")
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&b, " %s %d %s\n", digest(files[name]), len(files[name]), name)
	}
	b.WriteString("Files:\n")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[dir+dsc] = b.String()
	for name, body := range files {
		s.files[dir+name] = body
	}
}

func (s *sourceServer) requested(path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.asked {
		if strings.Contains(p, path) {
			return true
		}
	}

	return false
}

const sourcesHeader = "package\tarchitecture\tversion\tsource\tsource_version\torigin\tlicense\n"

// sourceFixture is a manifest, a server holding every source it needs, and the
// pin and review files the fetcher reads.
type sourceFixture struct {
	srv      *sourceServer
	manifest string
	pin      string
	reviewed string
	config   string
}

func newSourceFixture(t *testing.T) *sourceFixture {
	t.Helper()

	srv := newSourceServer(t)
	srv.debianSource("/ubuntu/+archive/primary/+sourcefiles/hello/2.10-3/", "hello_2.10-3.dsc", map[string]string{
		"hello_2.10.orig.tar.gz":     "hello upstream",
		"hello_2.10-3.debian.tar.xz": "hello packaging",
	})
	srv.debianSource("/ubuntu/+archive/primary/+sourcefiles/mystery/1.0-1/", "mystery_1.0-1.dsc", map[string]string{
		"mystery_1.0.orig.tar.gz": "mystery upstream",
	})
	srv.debianSource("/ubuntu/+archive/primary/+sourcefiles/foo/1:2.0-1/", "foo_2.0-1.dsc", map[string]string{
		"foo_2.0.orig.tar.gz": "foo under epoch 1",
	})
	srv.debianSource("/ubuntu/+archive/primary/+sourcefiles/foo/2:2.0-1/", "foo_2.0-1.dsc", map[string]string{
		"foo_2.0.orig.tar.gz": "foo under epoch 2",
	})
	srv.debianSource("/~git-core/+archive/ubuntu/ppa/+sourcefiles/git/1:2.55.0-0ppa1~ubuntu24.04.2/",
		"git_2.55.0-0ppa1~ubuntu24.04.2.dsc", map[string]string{
			"git_2.55.0.orig.tar.xz":                       "git upstream",
			"git_2.55.0-0ppa1~ubuntu24.04.2.debian.tar.xz": "git packaging",
		})
	srv.debianSource("/pool/main/p/postgresql-common/", "postgresql-common_293.pgdg24.04+1.dsc", map[string]string{
		"postgresql-common_293.pgdg24.04+1.tar.xz": "postgresql-common",
	})
	srv.debianSource("/pool/main/libp/libpgfoo/", "libpgfoo_1.2-1.pgdg24.04+1.dsc", map[string]string{
		"libpgfoo_1.2-1.pgdg24.04+1.tar.xz": "libpgfoo",
	})
	for path, body := range map[string]string{
		"/adoptium/temurin21-binaries/releases/download/jdk-21.0.12.1+1/OpenJDK21U-jdk-sources_21.0.12.1_1.tar.gz": "jdk21",
		"/adoptium/temurin17-binaries/releases/download/jdk-17.0.9+9/OpenJDK17U-jdk-sources_17.0.9_9.tar.gz":       "jdk17",
		"/adoptium/temurin8-binaries/releases/download/jdk8u504-b01/OpenJDK8U-jdk-sources_8u504b01.tar.gz":         "jdk8",
		"/linux-6.1.155.tar.xz": "linux",
	} {
		srv.files[path] = body
		if strings.HasSuffix(path, ".tar.gz") {
			srv.files[path+".sha256.txt"] = digest(body) + "  " + filepath.Base(path) + "\n"
		}
	}

	dir := t.TempDir()
	fx := &sourceFixture{
		srv:      srv,
		manifest: filepath.Join(dir, "IMAGE_SOURCES.tsv"),
		pin:      filepath.Join(dir, "linux.pin"),
		reviewed: filepath.Join(dir, "reviewed.tsv"),
		config:   filepath.Join(dir, "config"),
	}
	writeTreeFile(t, dir, "IMAGE_SOURCES.tsv", sourcesHeader+strings.Join([]string{
		"hello\tamd64\t2.10-3\thello\t2.10-3\tarchive.ubuntu.com\tgnu",
		"hello-doc\tall\t2.10-3\thello\t2.10-3\tarchive.ubuntu.com\tgnu",
		"git\tamd64\t1:2.55.0-0ppa1~ubuntu24.04.2\tgit\t1:2.55.0-0ppa1~ubuntu24.04.2\tnone\tgnu",
		"postgresql-common\tall\t293.pgdg24.04+1\tpostgresql-common\t293.pgdg24.04+1\tnone\tgnu",
		"libpgfoo1\tamd64\t1.2-1.pgdg24.04+1\tlibpgfoo\t1.2-1.pgdg24.04+1\tnone\tgnu",
		"temurin-21-jdk\tamd64\t21.0.12.1.0+1-0\ttemurin-21-jdk\t21.0.12.1.0+1-0\tnone\tgnu",
		"temurin-17-jdk\tamd64\t17.0.9.0.0+9\ttemurin-17-jdk\t17.0.9.0.0+9\tnone\tgnu",
		"temurin-8-jdk\tamd64\t8.0.504.0.0+1-0\ttemurin-8-jdk\t8.0.504.0.0+1-0\tnone\tgnu",
		"mystery\tamd64\t1.0-1\tmystery\t1.0-1\tarchive.ubuntu.com\tunclassified",
		"foo\tamd64\t1:2.0-1\tfoo\t1:2.0-1\tarchive.ubuntu.com\tgnu",
		"foo-tools\tamd64\t2:2.0-1\tfoo\t2:2.0-1\tarchive.ubuntu.com\tgnu",
		"gh\tamd64\t2.102.1\tgh\t2.102.1\tnone\tunclassified",
		"jq\tamd64\t1.7.1-3build1\tjq\t1.7.1-3build1\tarchive.ubuntu.com\tother",
	}, "\n")+"\n")
	writeTreeFile(t, dir, "linux.pin", "6.1.155 "+digest("linux")+"\n")
	writeTreeFile(t, dir, "reviewed.tsv", "source\tlicense\tevidence\treviewed\ngh\tMIT\thttps://example.test/LICENSE\t2026-10-21\n")
	writeTreeFile(t, dir, "config", "CONFIG_EXAMPLE=y\n")

	return fx
}

// run runs the fetcher against the fixture, and returns the directory it wrote,
// its output, and whether it succeeded.
func (fx *sourceFixture) run(t *testing.T, kernelVersion string) (string, string, bool) {
	t.Helper()

	return fx.runInto(t, kernelVersion, filepath.Join(t.TempDir(), "sources"))
}

// runInto runs the fetcher against the fixture into OUT.
func (fx *sourceFixture) runInto(t *testing.T, kernelVersion, out string) (string, string, bool) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "python3", "fetch-image-sources.py", fx.manifest, kernelVersion, fx.config, out)
	cmd.Env = append(os.Environ(),
		"BILLET_SOURCES_LAUNCHPAD="+fx.srv.URL,
		"BILLET_SOURCES_PGDG="+fx.srv.URL,
		"BILLET_SOURCES_ADOPTIUM="+fx.srv.URL,
		"BILLET_SOURCES_KERNEL="+fx.srv.URL,
		"BILLET_SOURCES_PIN="+fx.pin,
		"BILLET_SOURCES_REVIEWED="+fx.reviewed,
	)
	output, err := cmd.CombinedOutput()
	if exit := (*exec.ExitError)(nil); err != nil && !errors.As(err, &exit) {
		t.Fatalf("the fetcher did not run: %v\n%s", err, output)
	}

	return out, string(output), err == nil
}

// EVERY GNU-LICENSED SOURCE IS FETCHED AT ITS VERSION, FROM WHERE THAT VERSION IS
// KEPT, and verified: Ubuntu's archive and a PPA through Launchpad, the
// PostgreSQL archive by its pool, Adoptium's source tarball for a Temurin JDK, and
// the pinned kernel with its configuration. A package reviewed as under no GNU
// license and one classified other are left out; one unclassified and unreviewed
// is fetched.
func TestTheSourceArchiveHoldsEachSourceAtItsVersion(t *testing.T) {
	t.Parallel()

	fx := newSourceFixture(t)
	out, output, ok := fx.run(t, "6.1.155")
	if !ok {
		t.Fatalf("the fetcher failed:\n%s", output)
	}

	index, err := os.ReadFile(filepath.Join(out, "index.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	// THE INDEX ACCOUNTS FOR EVERY SOURCE THE MANIFEST NEEDS: each file fetched,
	// with the source and version it is of, and the reviewed one as reviewed.
	// The directory carries the whole version, epoch included, so foo's two
	// epochs are two sources.
	want := "source\tversion\tstatus\tfile\tsha256\tsize\n"
	for _, f := range []struct{ source, version, dir, name, body string }{
		{"foo", "1:2.0-1", "foo_1:2.0-1", "foo_2.0-1.dsc", ""},
		{"foo", "1:2.0-1", "foo_1:2.0-1", "foo_2.0.orig.tar.gz", "foo under epoch 1"},
		{"foo", "2:2.0-1", "foo_2:2.0-1", "foo_2.0-1.dsc", ""},
		{"foo", "2:2.0-1", "foo_2:2.0-1", "foo_2.0.orig.tar.gz", "foo under epoch 2"},
		{"gh", "2.102.1", "", "", ""},
		{"git", "1:2.55.0-0ppa1~ubuntu24.04.2", "git_1:2.55.0-0ppa1~ubuntu24.04.2", "git_2.55.0-0ppa1~ubuntu24.04.2.debian.tar.xz", "git packaging"},
		{"git", "1:2.55.0-0ppa1~ubuntu24.04.2", "git_1:2.55.0-0ppa1~ubuntu24.04.2", "git_2.55.0-0ppa1~ubuntu24.04.2.dsc", ""},
		{"git", "1:2.55.0-0ppa1~ubuntu24.04.2", "git_1:2.55.0-0ppa1~ubuntu24.04.2", "git_2.55.0.orig.tar.xz", "git upstream"},
		{"guest-kernel", "6.1.155", "guest-kernel_6.1.155", "config", "CONFIG_EXAMPLE=y\n"},
		{"guest-kernel", "6.1.155", "guest-kernel_6.1.155", "linux-6.1.155.tar.xz", "linux"},
		{"hello", "2.10-3", "hello_2.10-3", "hello_2.10-3.debian.tar.xz", "hello packaging"},
		{"hello", "2.10-3", "hello_2.10-3", "hello_2.10-3.dsc", ""},
		{"hello", "2.10-3", "hello_2.10-3", "hello_2.10.orig.tar.gz", "hello upstream"},
		{"libpgfoo", "1.2-1.pgdg24.04+1", "libpgfoo_1.2-1.pgdg24.04+1", "libpgfoo_1.2-1.pgdg24.04+1.dsc", ""},
		{"libpgfoo", "1.2-1.pgdg24.04+1", "libpgfoo_1.2-1.pgdg24.04+1", "libpgfoo_1.2-1.pgdg24.04+1.tar.xz", "libpgfoo"},
		{"mystery", "1.0-1", "mystery_1.0-1", "mystery_1.0-1.dsc", ""},
		{"mystery", "1.0-1", "mystery_1.0-1", "mystery_1.0.orig.tar.gz", "mystery upstream"},
		{"postgresql-common", "293.pgdg24.04+1", "postgresql-common_293.pgdg24.04+1", "postgresql-common_293.pgdg24.04+1.dsc", ""},
		{"postgresql-common", "293.pgdg24.04+1", "postgresql-common_293.pgdg24.04+1", "postgresql-common_293.pgdg24.04+1.tar.xz", "postgresql-common"},
		{"temurin-17-jdk", "17.0.9.0.0+9", "temurin-17-jdk_17.0.9.0.0+9", "OpenJDK17U-jdk-sources_17.0.9_9.tar.gz", "jdk17"},
		{"temurin-21-jdk", "21.0.12.1.0+1-0", "temurin-21-jdk_21.0.12.1.0+1-0", "OpenJDK21U-jdk-sources_21.0.12.1_1.tar.gz", "jdk21"},
		{"temurin-8-jdk", "8.0.504.0.0+1-0", "temurin-8-jdk_8.0.504.0.0+1-0", "OpenJDK8U-jdk-sources_8u504b01.tar.gz", "jdk8"},
	} {
		if f.dir == "" {
			want += f.source + "\t" + f.version + "\treviewed\t-\t-\t-\n"

			continue
		}
		path := filepath.Join(out, f.dir, f.name)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s/%s was not fetched: %v", f.dir, f.name, err)

			continue
		}
		if f.body != "" && string(body) != f.body {
			t.Errorf("%s/%s = %q, want %q", f.dir, f.name, body, f.body)
		}
		want += fmt.Sprintf("%s\t%s\tfetched\t%s/%s\t%s\t%d\n", f.source, f.version, f.dir, f.name, digest(string(body)), len(body))
	}
	if string(index) != want {
		t.Errorf("index.tsv is\n%s\nwant\n%s", index, want)
	}
	for _, never := range []string{"/gh/", "/jq/"} {
		if fx.srv.requested(never) {
			t.Errorf("the fetcher asked for %s, which needs no source", never)
		}
	}
}

// A SOURCE THAT CANNOT BE FETCHED AND VERIFIED FAILS THE RUN, naming it, and
// leaves no file that did not verify.
func TestTheSourceArchiveRefusesWhatItCannotVerify(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		stage  func(t *testing.T, fx *sourceFixture)
		kernel string
		want   string
		absent string
	}{
		{
			name: "a file that is not the one its .dsc describes",
			stage: func(t *testing.T, fx *sourceFixture) {
				t.Helper()

				fx.srv.files["/ubuntu/+archive/primary/+sourcefiles/hello/2.10-3/hello_2.10.orig.tar.gz"] = "tampered"
			},
			want:   "hello 2.10-3: hello_2.10.orig.tar.gz: sha256",
			absent: "hello_2.10-3/hello_2.10.orig.tar.gz",
		},
		{
			name: "a source the archive no longer has",
			stage: func(t *testing.T, fx *sourceFixture) {
				t.Helper()

				delete(fx.srv.files, "/~git-core/+archive/ubuntu/ppa/+sourcefiles/git/1:2.55.0-0ppa1~ubuntu24.04.2/git_2.55.0-0ppa1~ubuntu24.04.2.dsc")
			},
			want: "git 1:2.55.0-0ppa1~ubuntu24.04.2: ",
		},
		{
			name: "a Temurin tarball that is not the one its checksum names",
			stage: func(t *testing.T, fx *sourceFixture) {
				t.Helper()

				fx.srv.files["/adoptium/temurin8-binaries/releases/download/jdk8u504-b01/OpenJDK8U-jdk-sources_8u504b01.tar.gz"] = "other"
			},
			want:   "temurin-8-jdk 8.0.504.0.0+1-0: OpenJDK8U-jdk-sources_8u504b01.tar.gz: sha256",
			absent: "temurin-8-jdk_8.0.504.0.0+1-0/OpenJDK8U-jdk-sources_8u504b01.tar.gz",
		},
		{
			name:   "a kernel that is not the pinned one",
			kernel: "6.1.156",
			want:   "the image's kernel is 6.1.156 and scripts/kernel/linux.pin pins 6.1.155",
		},
		{
			name: "a kernel tarball that is not the pinned one",
			stage: func(t *testing.T, fx *sourceFixture) {
				t.Helper()

				fx.srv.files["/linux-6.1.155.tar.xz"] = "not linux"
			},
			want:   "linux 6.1.155: linux-6.1.155.tar.xz: sha256",
			absent: "guest-kernel_6.1.155/linux-6.1.155.tar.xz",
		},
		{
			name: "an unclassified package the review no longer names",
			stage: func(t *testing.T, fx *sourceFixture) {
				t.Helper()

				writeTreeFile(t, filepath.Dir(fx.reviewed), "reviewed.tsv", "source\tlicense\tevidence\treviewed\n")
			},
			want: "gh 2.102.1: ",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fx := newSourceFixture(t)
			if tc.stage != nil {
				tc.stage(t, fx)
			}
			kernel := tc.kernel
			if kernel == "" {
				kernel = "6.1.155"
			}
			out, output, ok := fx.run(t, kernel)
			if ok || !strings.Contains(output, tc.want) {
				t.Fatalf("the fetcher did not fail naming %q:\n%s", tc.want, output)
			}
			if _, err := os.Stat(filepath.Join(out, "index.tsv")); err == nil {
				t.Error("the fetcher wrote an index for an archive that did not verify")
			}
			if tc.absent != "" {
				if _, err := os.Stat(filepath.Join(out, tc.absent)); err == nil {
					t.Errorf("%s, which did not verify, was kept", tc.absent)
				}
			}
		})
	}
}

// A USED OUTPUT DIRECTORY IS REFUSED: what an earlier run left in it, a partial
// download or a source an older manifest needed, would be indexed and published.
func TestTheSourceArchiveStartsFromAnEmptyDirectory(t *testing.T) {
	t.Parallel()

	fx := newSourceFixture(t)
	out := t.TempDir()
	writeTreeFile(t, out, "stale_1.0/stale_1.0.orig.tar.gz.part", "left over")
	if _, output, ok := fx.runInto(t, "6.1.155", out); ok || !strings.Contains(output, out+" is not empty") {
		t.Fatalf("the fetcher ran into a used directory:\n%s", output)
	}
	if _, err := os.Stat(filepath.Join(out, "index.tsv")); err == nil {
		t.Error("the fetcher indexed a used directory")
	}
}

// THE PUBLISHER COMPARES SOURCE SETS IN BYTE ORDER whatever locale it inherits:
// its sorts and its comm must collate alike, or comm misses an intersection.
func TestGuestReleasePublisherComparesInByteOrder(t *testing.T) {
	t.Parallel()

	comm, err := exec.LookPath("comm")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	seen := filepath.Join(t.TempDir(), "locale")
	writeExecutable(t, filepath.Join(bin, "comm"), "#!/bin/sh\nprintf '%s' \"$LC_ALL\" >\""+seen+"\"\nexec "+comm+" \"$@\"\n")

	manifest := `{"schema": 1, "rootfs": {"name": "rootfs.img.zst", "sha256": "DIGEST:rootfs.img.zst", "size": 1},
  "kernel": {"name": "vmlinux-billet", "sha256": "DIGEST:vmlinux-billet", "size": 1, "version": "6.1.155"}}`
	out, ok, _ := runPublisher(t, publishDir(t, manifest, "rootfs.img.zst", "vmlinux-billet"), "PATH="+bin, "LC_ALL=en_US.UTF-8")
	if !ok {
		t.Fatalf("the publication failed:\n%s", out)
	}
	got, err := os.ReadFile(seen)
	if err != nil || string(got) != "C" {
		t.Fatalf("comm ran with LC_ALL=%q (%v), want C", got, err)
	}
}

// THE PPAS THE FETCHER KNOWS ARE THE TEMPLATE'S, and every source the review
// leaves out is under a license that asks for no source.
func TestTheSourceArchivesTablesAreTheTemplatesAndTheReviews(t *testing.T) {
	t.Parallel()

	fetcher := readScriptFile(t, "fetch-image-sources.py")
	install, err := os.ReadFile(filepath.Join("runner-images", "upstream", "images", "ubuntu", "scripts", "build", "install-git.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^GIT_REPO="ppa:([a-z0-9-]+)/([a-z0-9-]+)"$`).FindStringSubmatch(string(install))
	if m == nil {
		t.Fatal("install-git.sh no longer names its PPA as GIT_REPO=\"ppa:owner/name\"")
	}
	if want := `PPAS = {"git": "~` + m[1] + `/+archive/ubuntu/` + m[2] + `"}`; !strings.Contains(fetcher, "\n"+want+"\n") {
		t.Errorf("fetch-image-sources.py does not map git to the template's PPA (%s)", want)
	}

	reviewed, err := os.ReadFile(filepath.Join("runner-images", "reviewed-licenses.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(reviewed), "\n"), "\n")
	if len(lines) < 2 || lines[0] != "source\tlicense\tevidence\treviewed" {
		t.Fatalf("reviewed-licenses.tsv has no header or no row:\n%s", reviewed)
	}
	for _, line := range lines[1:] {
		cells := strings.Split(line, "\t")
		if len(cells) != 4 || !regexp.MustCompile(`^(Apache-2\.0|MIT|BSD-[23]-Clause)$`).MatchString(cells[1]) ||
			!strings.HasPrefix(cells[2], "https://") || !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`).MatchString(cells[3]) {
			t.Errorf("reviewed-licenses.tsv row %q is not a source, a license that asks for no source, its evidence and a date", line)
		}
	}
}

// publishIndex is the source index of the publication publishDir writes: its one
// GNU-licensed package, bash, fetched, and the guest kernel.
var publishIndex = "source\tversion\tstatus\tfile\tsha256\tsize\n" +
	"bash\t5.2.21-2ubuntu4\tfetched\tbash_5.2.21-2ubuntu4/bash_5.2.21.orig.tar.xz\t" + digest("bash") + "\t4\n" +
	"guest-kernel\t6.1.155\tfetched\tguest-kernel_6.1.155/linux-6.1.155.tar.xz\t" + digest("linux") + "\t5\n"

// writeSourceArchive writes into DIR the corresponding source a publication
// carries: a tar holding INDEX as ./index.tsv and a file, INDEX again as
// image-sources-index.tsv, and image-sources.parts naming the tar, its sha256
// and size, and, when PARTS is above zero, the tar split into that many parts.
func writeSourceArchive(t *testing.T, dir, index string, parts int) {
	t.Helper()

	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, f := range []struct{ name, body string }{
		{"./index.tsv", index},
		{"./bash_5.2.21-2ubuntu4/bash_5.2.21.orig.tar.xz", "bash"},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	archive := b.Bytes()

	list := fmt.Sprintf("image-sources.tar %s %d\n", digest(string(archive)), len(archive))
	write := func(name string, body []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("image-sources-index.tsv", []byte(index))
	if parts == 0 {
		write("image-sources.tar", archive)
	} else {
		step := (len(archive) + parts - 1) / parts
		for i := range parts {
			piece := archive[min(i*step, len(archive)):min((i+1)*step, len(archive))]
			name := fmt.Sprintf("image-sources.tar.part%03d", i)
			write(name, piece)
			list += name + " " + digest(string(piece)) + "\n"
		}
	}
	write("image-sources.parts", []byte(list))
}

// runPublisher runs publish-guest-release.sh against DIR with git and gh stubbed,
// and returns its output, whether it succeeded, and the calls it made. The calls
// end with release create's arguments, one per line, each prefixed "arg ", so a
// path split into two arguments shows as two lines.
func runPublisher(t *testing.T, dir string, env ...string) (string, bool, string) {
	t.Helper()

	publisher, err := filepath.Abs("publish-guest-release.sh")
	if err != nil {
		t.Fatal(err)
	}
	tools := t.TempDir()
	calls := filepath.Join(tools, "calls.log")
	writeExecutable(t, filepath.Join(tools, "git"), "#!/bin/sh\nprintf 'git %s\\n' \"$*\" >> \"$BILLET_TEST_CALLS\"\nexit 0\n")
	writeExecutable(t, filepath.Join(tools, "gh"), "#!/bin/sh\nprintf 'gh %s\\n' \"$*\" >> \"$BILLET_TEST_CALLS\"\n"+
		"if [ \"$1 $2\" = \"release create\" ]; then for a; do printf 'arg %s\\n' \"$a\" >> \"$BILLET_TEST_CALLS\"; done; fi\n"+
		"if [ \"$1 $2\" = \"release view\" ]; then printf 'true\\n'; exit 0; fi\nexit 0\n")
	cmd := exec.CommandContext(t.Context(), publisher, dir)
	cmd.Env = append(os.Environ(), "PATH="+tools+":"+os.Getenv("PATH"), "BILLET_TEST_CALLS="+calls,
		"GITHUB_REPOSITORY=junioryono/billet", "SOURCE_SHA="+strings.Repeat("a", 40), "TAG=guest-20260821-000000")
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			e = "PATH=" + strings.TrimPrefix(e, "PATH=") + ":" + tools + ":" + os.Getenv("PATH")
		}
		cmd.Env = append(cmd.Env, e)
	}
	out, err := cmd.CombinedOutput()
	if exit := (*exec.ExitError)(nil); err != nil && !errors.As(err, &exit) {
		t.Fatalf("the publisher did not run: %v\n%s", err, out)
	}
	logged, readErr := os.ReadFile(calls)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		t.Fatal(readErr)
	}

	return string(out), err == nil, string(logged)
}

// THE PUBLISHER HOLDS THE CORRESPONDING SOURCE WHOLE BEFORE IT TAGS: the parts the
// list names, joined in order, must be the archive its first line describes; the
// published index must be the one inside it; and the index must account for
// exactly the sources the manifest needs, the kernel's once.
func TestGuestReleasePublisherHoldsTheSourceArchiveWhole(t *testing.T) {
	t.Parallel()

	manifest := `{"schema": 1, "rootfs": {"name": "rootfs.img.zst", "sha256": "DIGEST:rootfs.img.zst", "size": 1},
  "kernel": {"name": "vmlinux-billet", "sha256": "DIGEST:vmlinux-billet", "size": 1, "version": "6.1.155"}}`
	fresh := func(t *testing.T) string {
		t.Helper()

		return publishDir(t, manifest, "rootfs.img.zst", "vmlinux-billet")
	}
	split := func(t *testing.T, parts int) string {
		t.Helper()

		dir := fresh(t)
		removeAll(t, filepath.Join(dir, "image-sources.tar"))
		writeSourceArchive(t, dir, publishIndex, parts)

		return dir
	}
	edit := func(t *testing.T, dir, name string, change func(string) string) string {
		t.Helper()

		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		changed := change(string(body))
		if changed == string(body) {
			t.Fatalf("the edit left %s unchanged", name)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(changed), 0o600); err != nil {
			t.Fatal(err)
		}

		return dir
	}
	indexLines := strings.Split(strings.TrimSuffix(publishIndex, "\n"), "\n")

	for _, tc := range []struct {
		name    string
		dir     func(t *testing.T) string
		env     func(t *testing.T) []string
		ok      bool
		want    string
		uploads []string
		absent  []string
	}{
		{name: "one archive", dir: fresh, ok: true, uploads: []string{"/image-sources.tar", "/image-sources.parts", "/image-sources-index.tsv"}},
		{
			name: "an archive in parts", ok: true,
			dir: func(t *testing.T) string {
				t.Helper()

				return split(t, 3)
			},
			uploads: []string{"/image-sources.tar.part000", "/image-sources.tar.part001", "/image-sources.tar.part002"},
			absent:  []string{"/image-sources.tar"},
		},
		{
			name: "a list with no final newline", ok: true,
			dir: func(t *testing.T) string {
				t.Helper()

				return edit(t, split(t, 2), "image-sources.parts", func(s string) string { return strings.TrimSuffix(s, "\n") })
			},
			uploads: []string{"/image-sources.tar.part001"},
		},
		{
			name: "a list leaving out the last part",
			dir: func(t *testing.T) string {
				t.Helper()

				return edit(t, split(t, 3), "image-sources.parts", func(s string) string {
					kept, _, found := strings.Cut(s, "image-sources.tar.part002")
					if !found {
						t.Fatal("the fixture's list has no third part")
					}

					return kept
				})
			},
		},
		{
			name: "parts listed out of order",
			dir: func(t *testing.T) string {
				t.Helper()

				return edit(t, split(t, 2), "image-sources.parts", func(s string) string {
					lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")

					return lines[0] + "\n" + lines[2] + "\n" + lines[1] + "\n"
				})
			},
		},
		{
			// THE PART'S CONTENT IS THE WHOLE ARCHIVE, digest and all, so only the
			// name check stands between it and the release.
			name: "a part under a name not the archive's",
			dir: func(t *testing.T) string {
				t.Helper()

				dir := fresh(t)
				body, err := os.ReadFile(filepath.Join(dir, "image-sources.tar"))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "elsewhere.tar"), body, 0o600); err != nil {
					t.Fatal(err)
				}
				removeAll(t, filepath.Join(dir, "image-sources.tar"))

				return edit(t, dir, "image-sources.parts", func(s string) string {
					return s + "elsewhere.tar " + digest(string(body)) + "\n"
				})
			},
		},
		{
			name: "a list of nothing but a space",
			dir: func(t *testing.T) string {
				t.Helper()

				return rewritten(t, fresh(t), "image-sources.parts", " ")
			},
		},
		{
			name: "an archive that is not the one the list describes",
			dir: func(t *testing.T) string {
				t.Helper()

				return edit(t, fresh(t), "image-sources.tar", func(s string) string { return s + "x" })
			},
		},
		{
			name: "a published index that is not the archive's",
			dir: func(t *testing.T) string {
				t.Helper()

				return edit(t, fresh(t), "image-sources-index.tsv", func(s string) string {
					return s + "bash\t5.2.21-2ubuntu4\tfetched\tbash_5.2.21-2ubuntu4/extra\t" + digest("x") + "\t1\n"
				})
			},
		},
		{
			name: "an index without a source the manifest needs",
			dir: func(t *testing.T) string {
				t.Helper()

				dir := fresh(t)
				writeSourceArchive(t, dir, indexLines[0]+"\n"+indexLines[2]+"\n", 0)

				return dir
			},
		},
		{
			name: "a GNU-licensed source marked reviewed",
			dir: func(t *testing.T) string {
				t.Helper()

				dir := fresh(t)
				writeSourceArchive(t, dir, indexLines[0]+"\nbash\t5.2.21-2ubuntu4\treviewed\t-\t-\t-\n"+indexLines[2]+"\n", 0)

				return dir
			},
		},
		{
			// THE OUTPUT DIRECTORY'S OWN PATH IS ANYTHING: only the part names
			// are checked, and the archive is read from inside it.
			name: "an archive in parts under a directory whose path has a space", ok: true,
			dir: func(t *testing.T) string {
				t.Helper()

				dir := split(t, 2)
				spaced := filepath.Join(t.TempDir(), "release files")
				if err := os.Rename(dir, spaced); err != nil {
					t.Fatal(err)
				}

				return spaced
			},
			uploads: []string{"/release files/image-sources.tar.part000", "/release files/image-sources.tar.part001"},
		},
		{
			name: "the source of a kernel other than the image's",
			dir: func(t *testing.T) string {
				t.Helper()

				return edit(t, fresh(t), "manifest.json", func(s string) string {
					return strings.Replace(s, `"version": "6.1.155"`, `"version": "6.1.156"`, 1)
				})
			},
		},
		{
			name: "a manifest naming no kernel version",
			dir: func(t *testing.T) string {
				t.Helper()

				return edit(t, fresh(t), "manifest.json", func(s string) string {
					return strings.Replace(s, `, "version": "6.1.155"`, ``, 1)
				})
			},
		},
		{
			// AN awk THAT FAILS INTO A SORT IS AN EMPTY SET under dash, and an
			// empty set of reviewed sources hides the GNU one marked reviewed.
			name: "an extraction that fails beside a GNU-licensed source marked reviewed",
			// THE REFUSAL IS THE EXTRACTION'S, not the comparison's that would
			// refuse this index anyway.
			want: "could not read the sources image-sources.tsv needs",
			dir: func(t *testing.T) string {
				t.Helper()

				dir := fresh(t)
				writeSourceArchive(t, dir, indexLines[0]+"\nbash\t5.2.21-2ubuntu4\treviewed\t-\t-\t-\n"+indexLines[2]+"\n", 0)

				return dir
			},
			env: func(t *testing.T) []string {
				t.Helper()

				awk, err := exec.LookPath("awk")
				if err != nil {
					t.Fatal(err)
				}
				bin := t.TempDir()
				writeExecutable(t, filepath.Join(bin, "awk"), "#!/bin/sh\n"+
					"case \"$*\" in *'$3 == \"reviewed\" {'*) exit 2 ;; esac\nexec "+awk+" \"$@\"\n")

				return []string{"PATH=" + bin}
			},
		},
		{
			// A comm THAT FAILS SAYS NOTHING, and nothing is not "no GNU source
			// marked reviewed".
			name: "a comparison that fails",
			dir:  fresh,
			env: func(t *testing.T) []string {
				t.Helper()

				bin := t.TempDir()
				writeExecutable(t, filepath.Join(bin, "comm"), "#!/bin/sh\nexit 2\n")

				return []string{"PATH=" + bin}
			},
		},
		{
			name: "one of two GNU-licensed sources marked reviewed",
			dir: func(t *testing.T) string {
				t.Helper()

				dir := fresh(t)
				dir = edit(t, dir, "image-sources.tsv", func(s string) string {
					return s + "zlib1g\tamd64\t1:1.3\tzlib\t1:1.3\tarchive.ubuntu.com\tgnu\n"
				})
				writeSourceArchive(t, dir, indexLines[0]+"\n"+indexLines[1]+"\n"+indexLines[2]+"\nzlib\t1:1.3\treviewed\t-\t-\t-\n", 0)

				return dir
			},
		},
		{
			name: "an index without the kernel's source",
			dir: func(t *testing.T) string {
				t.Helper()

				dir := fresh(t)
				writeSourceArchive(t, dir, indexLines[0]+"\n"+indexLines[1]+"\n", 0)

				return dir
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var env []string
			if tc.env != nil {
				env = tc.env(t)
			}
			out, ok, calls := runPublisher(t, tc.dir(t), env...)
			if ok != tc.ok {
				t.Fatalf("publication success = %v, want %v\n%s\n%s", ok, tc.ok, out, calls)
			}
			if !tc.ok {
				if tc.want != "" && !strings.Contains(out, tc.want) {
					t.Errorf("the publication was refused without saying %q:\n%s", tc.want, out)
				}
				for _, call := range []string{"git tag", "git push", "release create"} {
					if strings.Contains(calls, call) {
						t.Errorf("a refused publication still ran %q:\n%s", call, calls)
					}
				}

				return
			}
			var args []string
			for _, line := range strings.Split(calls, "\n") {
				if arg, ok := strings.CutPrefix(line, "arg "); ok {
					args = append(args, arg)
				}
			}
			uploaded := func(suffix string) bool {
				for _, arg := range args {
					if strings.HasSuffix(arg, suffix) {
						return true
					}
				}

				return false
			}
			for _, want := range tc.uploads {
				if !uploaded(want) {
					t.Errorf("the release was created without an argument ending %s:\n%s", want, strings.Join(args, "\n"))
				}
			}
			for _, never := range tc.absent {
				if uploaded(never) {
					t.Errorf("the release was created with an argument ending %s:\n%s", never, strings.Join(args, "\n"))
				}
			}
		})
	}
}
