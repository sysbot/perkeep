/*
Copyright 2014 The Camlistore Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package blobpacked

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"math/rand"
	"runtime"
	"testing"
	"time"

	"camlistore.org/pkg/blob"
	"camlistore.org/pkg/blobserver"
	"camlistore.org/pkg/blobserver/storagetest"
	"camlistore.org/pkg/constants"
	"camlistore.org/pkg/context"
	"camlistore.org/pkg/schema"
	"camlistore.org/pkg/sorted"
	"camlistore.org/pkg/syncutil"
	"camlistore.org/pkg/test"
	"camlistore.org/third_party/go/pkg/archive/zip"
)

const debug = false

var brokenTests = flag.Bool("broken", false, "also test known-broken tests")

func TestStorage(t *testing.T) {
	storagetest.Test(t, func(t *testing.T) (sto blobserver.Storage, cleanup func()) {
		s := &storage{
			small: new(test.Fetcher),
			large: new(test.Fetcher),
			meta:  sorted.NewMemoryKeyValue(),
			log:   test.NewLogger(t, "blobpacked: "),
		}
		s.init()
		return s, func() {}
	})
}

func TestParseMetaRow(t *testing.T) {
	cases := []struct {
		in   string
		want meta
		err  bool
	}{
		{in: "123 sx", err: true},
		{in: "-123 s", err: true},
		{in: "", err: true},
		{in: "1 ", err: true},
		{in: " ", err: true},
		{in: "123 x", err: true},
		{in: "123 l", err: true},
		{in: "123 sha1-f1d2d2f924e986ac86fdf7b36c94bcdf32beec15", err: true},
		{in: "123 notaref 12", err: true},
		{in: "123 sha1-f1d2d2f924e986ac86fdf7b36c94bcdf32beec15 42 extra", err: true},
		{in: "123 sha1-f1d2d2f924e986ac86fdf7b36c94bcdf32beec15 42 ", err: true},
		{in: "123 sha1-f1d2d2f924e986ac86fdf7b36c94bcdf32beec15 42", want: meta{
			exists:   true,
			size:     123,
			largeRef: blob.MustParse("sha1-f1d2d2f924e986ac86fdf7b36c94bcdf32beec15"),
			largeOff: 42,
		}},
	}
	for _, tt := range cases {
		got, err := parseMetaRow([]byte(tt.in))
		if (err != nil) != tt.err {
			t.Errorf("For %q error = %v; want-err? = %v", tt.in, err, tt.err)
			continue
		}
		if tt.err {
			continue
		}
		if got != tt.want {
			t.Errorf("For %q, parseMetaRow = %+v; want %+v", tt.in, got, tt.want)
		}
	}
}

func wantNumLargeBlobs(want int) func(*packTest) {
	return func(pt *packTest) { pt.wantLargeBlobs = want }
}

func wantNumSmallBlobs(want int) func(*packTest) {
	return func(pt *packTest) { pt.wantSmallBlobs = want }
}

func okayWithoutMeta(refStr string) func(*packTest) {
	return func(pt *packTest) {
		if pt.okayNoMeta == nil {
			pt.okayNoMeta = map[blob.Ref]bool{}
		}
		pt.okayNoMeta[blob.MustParse(refStr)] = true
	}
}

func randBytes(n int) []byte {
	r := rand.New(rand.NewSource(42))
	s := make([]byte, n)
	for i := range s {
		s[i] = byte(r.Int63())
	}
	return s
}

func TestPackNormal(t *testing.T) {
	const fileSize = 5 << 20
	const fileName = "foo.dat"
	fileContents := randBytes(fileSize)
	testPack(t,
		func(sto blobserver.Storage) error {
			_, err := schema.WriteFileFromReader(sto, fileName, bytes.NewReader(fileContents))
			return err
		},
		wantNumLargeBlobs(1),
		wantNumSmallBlobs(0),
	)
}

func TestPackNoDelete(t *testing.T) {
	const fileSize = 1 << 20
	const fileName = "foo.dat"
	fileContents := randBytes(fileSize)
	testPack(t,
		func(sto blobserver.Storage) error {
			_, err := schema.WriteFileFromReader(sto, fileName, bytes.NewReader(fileContents))
			return err
		},
		func(pt *packTest) { pt.sto.skipDelete = true },
		wantNumLargeBlobs(1),
		wantNumSmallBlobs(15), // empirically
	)
}

func TestPackLarge(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	const fileSize = 17 << 20 // more than 16 MB, so more than one zip
	const fileName = "foo.dat"
	fileContents := randBytes(fileSize)
	testPack(t,
		func(sto blobserver.Storage) error {
			_, err := schema.WriteFileFromReader(sto, fileName, bytes.NewReader(fileContents))
			return err
		},
		wantNumLargeBlobs(2),
		wantNumSmallBlobs(0),
	)
}

func TestPackTwoIdenticalfiles(t *testing.T) {
	const fileSize = 1 << 20
	fileContents := randBytes(fileSize)
	testPack(t,
		func(sto blobserver.Storage) (err error) {
			if _, err = schema.WriteFileFromReader(sto, "a.txt", bytes.NewReader(fileContents)); err != nil {
				return
			}
			if _, err = schema.WriteFileFromReader(sto, "b.txt", bytes.NewReader(fileContents)); err != nil {
				return
			}
			return
		},
		func(pt *packTest) { pt.sto.packGate = syncutil.NewGate(1) }, // one pack at a time
		wantNumLargeBlobs(1),
		wantNumSmallBlobs(1), // just the "b.txt" file schema blob
		okayWithoutMeta("sha1-cb4399f6b3b31ace417e1ec9326f9818bb3f8387"),
	)
}

// packTest is the state kept while running func testPack.
type packTest struct {
	sto                   *storage
	logical, small, large *test.Fetcher

	wantLargeBlobs interface{} // nil means disabled, else int
	wantSmallBlobs interface{} // nil means disabled, else int

	okayNoMeta map[blob.Ref]bool
}

func testPack(t *testing.T,
	write func(sto blobserver.Storage) error,
	checks ...func(*packTest),
) {
	ctx := context.New()
	defer ctx.Cancel()

	logical := new(test.Fetcher)
	small, large := new(test.Fetcher), new(test.Fetcher)
	pt := &packTest{
		logical: logical,
		small:   small,
		large:   large,
	}
	// Figure out the logical baseline blobs we'll later expect in the packed storage.
	if err := write(logical); err != nil {
		t.Fatal(err)
	}
	t.Logf("items in logical storage: %d", logical.NumBlobs())

	pt.sto = &storage{
		small: small,
		large: large,
		meta:  sorted.NewMemoryKeyValue(),
		log:   test.NewLogger(t, "blobpacked: "),
	}
	pt.sto.init()

	for _, setOpt := range checks {
		setOpt(pt)
	}

	if err := write(pt.sto); err != nil {
		t.Fatal(err)
	}

	t.Logf("items in small: %v", small.NumBlobs())
	t.Logf("items in large: %v", large.NumBlobs())

	it := pt.sto.meta.Find("", "")
	skipPrefix := []byte("b:")
	for it.Next() {
		if bytes.HasPrefix(it.KeyBytes(), skipPrefix) {
			// boring row
			continue
		}
		t.Logf("meta %q = %q", it.KeyBytes(), it.ValueBytes())
	}
	if err := it.Close(); err != nil {
		t.Fatal(err)
	}

	if want, ok := pt.wantLargeBlobs.(int); ok && want != large.NumBlobs() {
		t.Fatalf("num large blobs = %d; want %d", large.NumBlobs(), want)
	}
	if want, ok := pt.wantSmallBlobs.(int); ok && want != small.NumBlobs() {
		t.Fatalf("num small blobs = %d; want %d", small.NumBlobs(), want)
	}

	var zipRefs []blob.Ref
	var zipSeen = map[blob.Ref]bool{}
	blobserver.EnumerateAll(ctx, large, func(sb blob.SizedRef) error {
		zipRefs = append(zipRefs, sb.Ref)
		zipSeen[sb.Ref] = true
		return nil
	})
	if len(zipRefs) != large.NumBlobs() {
		t.Fatalf("Enumerated only %d zip files; expected %d", len(zipRefs), large.NumBlobs())
	}

	bytesOfZip := map[blob.Ref][]byte{}
	for _, zipRef := range zipRefs {
		rc, _, err := large.Fetch(zipRef)
		if err != nil {
			t.Fatal(err)
		}
		zipBytes, err := ioutil.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("Error slurping %s: %v", zipRef, err)
		}
		if len(zipBytes) > constants.MaxBlobSize {
			t.Fatalf("zip is too large: %d > max %d", len(zipBytes), constants.MaxBlobSize)
		}
		bytesOfZip[zipRef] = zipBytes
		zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
		if err != nil {
			t.Fatalf("Error reading resulting zip file: %v", err)
		}
		if len(zr.File) == 0 {
			t.Fatal("zip is empty")
		}
		nameSeen := map[string]bool{}
		for i, zf := range zr.File {
			if nameSeen[zf.Name] {
				t.Errorf("duplicate name %q seen", zf.Name)
			}
			nameSeen[zf.Name] = true
			t.Logf("zip[%d] size %d, %v", i, zf.UncompressedSize64, zf.Name)
		}
		mfr, err := zr.File[len(zr.File)-1].Open()
		if err != nil {
			t.Fatalf("Error opening manifest JSON: %v", err)
		}
		maniJSON, err := ioutil.ReadAll(mfr)
		if err != nil {
			t.Fatalf("Error reading manifest JSON: %v", err)
		}
		var mf Manifest
		if err := json.Unmarshal(maniJSON, &mf); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}

		// Verify each chunk described in the manifest:
		baseOffset, err := zr.File[0].DataOffset()
		if err != nil {
			t.Fatal(err)
		}
		for _, bo := range mf.DataBlobs {
			h := bo.Ref.Hash()
			h.Write(zipBytes[baseOffset+bo.Offset : baseOffset+bo.Offset+int64(bo.Size)])
			if !bo.Ref.HashMatches(h) {
				t.Errorf("blob %+v didn't describe the actual data in the zip", bo)
			}
		}
		if debug {
			t.Logf("Manifest: %s", maniJSON)
		}
	}

	// Verify that each chunk in the logical mapping is in the meta.
	logBlobs := 0
	if err := blobserver.EnumerateAll(ctx, logical, func(sb blob.SizedRef) error {
		logBlobs++
		v, err := pt.sto.meta.Get(blobMetaPrefix + sb.Ref.String())
		if err == sorted.ErrNotFound && pt.okayNoMeta[sb.Ref] {
			return nil
		}
		if err != nil {
			return fmt.Errorf("error looking up logical blob %v in meta: %v", sb.Ref, err)
		}
		m, err := parseMetaRow([]byte(v))
		if err != nil {
			return fmt.Errorf("error parsing logical blob %v meta %q: %v", sb.Ref, v, err)
		}
		if !m.exists || m.size != sb.Size || !zipSeen[m.largeRef] {
			return fmt.Errorf("logical blob %v = %+v; want in zip", sb.Ref, m)
		}
		h := sb.Ref.Hash()
		h.Write(bytesOfZip[m.largeRef][m.largeOff : m.largeOff+sb.Size])
		if !sb.Ref.HashMatches(h) {
			t.Errorf("blob %v not found matching in zip", sb.Ref)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if logBlobs != logical.NumBlobs() {
		t.Error("enumerate over logical blobs didn't work?")
	}

	// TODO: so many more tests:

	// -- like TestPackTwoIdenticalfiles, but instead of testing
	// no dup for 100% identical file bytes, test that uploading a
	// 49% identical one does denormalize and repack.
	// -- verify deleting from the source
	// -- verify we can reconstruct it all from the zip
	// -- verify the meta before & after
	// -- verify we can still get each blob. and enumerate.
	// -- test StreamBlobs in all its various flavours, and recovering from stream blobs.
	// -- overflowing the 16MB chunk size with huge initial chunks
}

// see if storage proxies through to small for Fetch, Stat, and Enumerate.
func TestSmallFallback(t *testing.T) {
	small := new(test.Fetcher)
	s := &storage{
		small: small,
		large: new(test.Fetcher),
		meta:  sorted.NewMemoryKeyValue(),
		log:   test.NewLogger(t, "blobpacked: "),
	}
	s.init()
	b1 := &test.Blob{"foo"}
	b1.MustUpload(t, small)
	wantSB := b1.SizedRef()

	// Fetch
	rc, _, err := s.Fetch(b1.BlobRef())
	if err != nil {
		t.Errorf("failed to Get blob: %v", err)
	} else {
		rc.Close()
	}

	// Stat.
	sb, err := blobserver.StatBlob(s, b1.BlobRef())
	if err != nil {
		t.Errorf("failed to Stat blob: %v", err)
	} else if sb != wantSB {
		t.Errorf("Stat = %v; want %v", sb, wantSB)
	}

	// Enumerate
	saw := false
	ctx := context.New()
	defer ctx.Cancel()
	if err := blobserver.EnumerateAll(ctx, s, func(sb blob.SizedRef) error {
		if sb != wantSB {
			return fmt.Errorf("saw blob %v; want %v", sb, wantSB)
		}
		saw = true
		return nil
	}); err != nil {
		t.Errorf("EnuerateAll: %v", err)
	}
	if !saw {
		t.Error("didn't see blob in Enumerate")
	}
}

func TestZ_LeakCheck(t *testing.T) {
	if testing.Short() {
		return
	}
	time.Sleep(50 * time.Millisecond) // let goroutines schedule & die off
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	n := bytes.Count(buf, []byte("[chan receive]:"))
	if n > 1 {
		t.Errorf("%d goroutines in chan receive: %s", n, buf)
	}
}