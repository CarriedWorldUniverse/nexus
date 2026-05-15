// Broker-mediated aspect mint (NEX-134). Client side.
//
// Talks to /api/admin/aspects/mint on a running broker. Aspect keypair
// is generated locally; only the public key + metadata travel to the
// broker. Privkey never leaves the operator's machine. After the broker
// confirms the row, the CLI seals the privkey against the broker's
// server pubkey and writes the keyfile locally — same end state as
// the offline path, but the broker stays the single DB writer.

package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/CarriedWorldUniverse/nexus/nexus/aspects"
	"github.com/CarriedWorldUniverse/nexus/nexus/broker"
)

func runAspectMintViaBroker(name, outPath, nexusURL, brokerURL, adminToken, provider, model string, force bool) int {
	// Generate the aspect keypair locally so the privkey never leaves
	// this machine. The broker only ever sees the pubkey.
	aspectPub, aspectPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aspect mint: generate keypair: %v\n", err)
		return 1
	}

	reqBody := broker.AdminMintRequest{
		Name:            name,
		AspectPubkeyB64: base64.StdEncoding.EncodeToString(aspectPub),
		Provider:        provider,
		Model:           model,
		Force:           force,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aspect mint: marshal request: %v\n", err)
		return 1
	}

	endpoint := strings.TrimRight(brokerURL, "/") + "/api/admin/aspects/mint"
	httpReq, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		fmt.Fprintf(os.Stderr, "aspect mint: build request: %v\n", err)
		return 1
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+adminToken)

	// 30s wall-clock cap. The mint itself is cheap; this covers the
	// TLS handshake + admin auth + a single DB write. Self-signed
	// certs are the operator-side norm (tailnet hostnames with
	// Tailscale-issued certs), so InsecureSkipVerify is conservatively
	// opt-in via the broker URL itself — operators dialing a CA-signed
	// host get normal verification; tailnet-internal scenarios pass
	// the cert through the system trust store.
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			// Honor NEXUS_INSECURE_TLS for tailnet self-signed flows.
			// Standard https URLs verify normally.
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: os.Getenv("NEXUS_INSECURE_TLS") == "1",
			},
		},
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aspect mint: POST %s: %v\n", endpoint, err)
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "aspect mint: broker returned %d: %s\n", resp.StatusCode, strings.TrimSpace(string(respBody)))
		return 1
	}

	var mintResp broker.AdminMintResponse
	if err := json.NewDecoder(resp.Body).Decode(&mintResp); err != nil {
		fmt.Fprintf(os.Stderr, "aspect mint: decode broker response: %v\n", err)
		return 1
	}

	serverPub, err := base64.StdEncoding.DecodeString(mintResp.ServerPubkeyB64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aspect mint: decode server pubkey: %v\n", err)
		return 1
	}

	if mintResp.PreviouslyExisted && !force {
		fmt.Fprintf(os.Stderr, "WARNING: %q already existed at the broker; re-minted at version %d — previous keyfile is permanently invalid after this bump.\n",
			name, mintResp.KeyfileVersion)
		fmt.Fprintln(os.Stderr, "Pass --force to suppress this notice in scripted use.")
	}

	// Seal the privkey against the broker's server pubkey and write the
	// keyfile locally. Identical to the offline path's final step.
	kf, fingerprint, err := aspects.Mint(aspects.MintInput{
		AspectName:     name,
		KeyfileVersion: mintResp.KeyfileVersion,
		AspectPrivkey:  aspectPriv,
		ServerPubkey:   ed25519.PublicKey(serverPub),
		NexusID:        mintResp.NexusID,
		NexusURL:       nexusURL,
		MintedAt:       time.Now().UTC(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "aspect mint: build keyfile: %v\n", err)
		return 1
	}

	body, err := json.MarshalIndent(kf, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "aspect mint: marshal keyfile: %v\n", err)
		return 1
	}
	if err := writeKeyfile(outPath, body); err != nil {
		fmt.Fprintf(os.Stderr, "aspect mint: write %s: %v\n", outPath, err)
		return 1
	}

	fmt.Printf("aspect: %s\n", name)
	fmt.Printf("keyfile_version: %d\n", mintResp.KeyfileVersion)
	fmt.Printf("fingerprint (sha256 of sealed payload): %s\n", fingerprint)
	fmt.Printf("nexus_id: %s\n", mintResp.NexusID)
	fmt.Printf("written: %s (mode 0600)\n", outPath)
	fmt.Printf("via broker: %s\n", brokerURL)
	fmt.Println()
	fmt.Println("Distribute this file like an SSH private key.")
	fmt.Println("Re-minting bumps the version and invalidates this file.")
	return 0
}
