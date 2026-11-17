// Command release-sign signs a release (SPEC 2 "Release channel"): it reads
// the SHA256SUMS scripts/release wrote and writes manifest.json and
// manifest.json.sig beside it, signed with the Ed25519 key in
// ZM_RELEASE_KEY (base64 of the 32-byte seed). -keygen prints a new key
// pair: the seed for the CI secret, the public key for config update_keys.
//
//	go run ./scripts/release-sign -keygen
//	ZM_RELEASE_KEY=… go run ./scripts/release-sign -version main-1eacf23e722f -commit <sha> [-out dist]
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/release"
)

func main() {
	keygen := flag.Bool("keygen", false, "print a new key pair and exit")
	version := flag.String("version", "", "the release version (as scripts/release stamped it)")
	commit := flag.String("commit", "", "the full commit it was built from")
	out := flag.String("out", "dist", "the directory holding SHA256SUMS")
	flag.Parse()
	if err := run(*keygen, *version, *commit, *out); err != nil {
		fmt.Fprintln(os.Stderr, "release-sign:", err)
		os.Exit(1)
	}
}

func run(keygen bool, version, commit, out string) error {
	if keygen {
		pub, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			return err
		}
		fmt.Println("secret ZM_RELEASE_KEY:", base64.StdEncoding.EncodeToString(priv.Seed()))
		fmt.Println("update_keys entry:    ", release.FormatKey(pub))
		return nil
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("ZM_RELEASE_KEY")))
	if err != nil || len(seed) != ed25519.SeedSize {
		return fmt.Errorf("ZM_RELEASE_KEY must be base64 of a %d-byte Ed25519 seed", ed25519.SeedSize)
	}
	key := ed25519.NewKeyFromSeed(seed)
	sums, err := os.ReadFile(filepath.Join(out, "SHA256SUMS"))
	if err != nil {
		return err
	}
	m, err := release.FromSums(version, commit, time.Now(), sums)
	if err != nil {
		return err
	}
	body, sig, err := release.Sign(m, key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, release.ManifestFile), body, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, release.SignatureFile), []byte(sig+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Printf("signed %s (%d files) with %s\n", m.Version, len(m.Files), release.FormatKey(key.Public().(ed25519.PublicKey)))
	return nil
}
