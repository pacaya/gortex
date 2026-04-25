# Installing Gortex

Pre-built binaries are published to [GitHub Releases](https://github.com/zzet/gortex/releases) for linux/amd64, linux/arm64, darwin/amd64 (Intel Mac), and darwin/arm64 (Apple Silicon). Every release is **cosign-signed**, ships **SLSA-3 provenance**, and is **VirusTotal-scanned** — see [Verifying releases](#verifying-releases-supply-chain-security) below. Windows support is planned.

**New to Gortex?** After installing, see [onboarding.md](onboarding.md) for the 15-minute walkthrough: `gortex install` (once per machine) → `gortex init` (once per repo) → verify your AI assistant uses graph tools → what to do if it doesn't.

## One-line install (Linux / macOS)

```bash
curl -fsSL https://get.gortex.dev | sh
```

Detects OS/arch, downloads the signed release tarball, verifies the SHA256 against `checksums.txt` (and the cosign signature if `cosign` is installed), drops the binary in `$HOME/.local/bin`, and adds that directory to your shell rc (`.zshrc` / `.bashrc` / fish config) inside an idempotent `# >>> gortex installer >>>` block. Re-runs upgrade in place and back up the previous binary as `gortex.previous`. No silent sudo.

**On macOS, if `brew` is on `PATH`, the script delegates to Homebrew automatically** (`brew install zzet/tap/gortex`, or `brew upgrade --cask gortex` on re-runs) so updates flow through `brew upgrade`. Set `GORTEX_NO_BREW=1` to force the tarball path instead.

Override defaults via env vars: `GORTEX_VERSION=v0.15.0` (pin a version — also disables Homebrew), `GORTEX_INSTALL_DIR=/usr/local/bin` (system-wide; you'll need write permission — also disables Homebrew), `GORTEX_NO_BREW=1` (skip Homebrew on macOS), `GORTEX_NO_PATH=1` (skip rc edits), `GORTEX_NO_VERIFY=1` (skip checksum + cosign), `GORTEX_FORCE=1` (overwrite without backup). Source: [`scripts/install.sh`](../scripts/install.sh).

## macOS — Homebrew

```bash
brew install zzet/tap/gortex
```

Homebrew strips the `homebrew-` prefix from tap repositories, so `zzet/homebrew-tap` is installed as `zzet/tap`. Updates via `brew upgrade`. No Gatekeeper prompt — `brew` doesn't set the quarantine attribute on downloads.

## Linux — Debian / Ubuntu (.deb)

```bash
ARCH=$(dpkg --print-architecture)  # amd64 or arm64
curl -LO "https://github.com/zzet/gortex/releases/latest/download/gortex_linux_${ARCH}.deb"
sudo dpkg -i "gortex_linux_${ARCH}.deb"
```

## Linux — RHEL / Fedora / CentOS (.rpm)

```bash
ARCH=$(uname -m); [ "$ARCH" = x86_64 ] && ARCH=amd64; [ "$ARCH" = aarch64 ] && ARCH=arm64
curl -LO "https://github.com/zzet/gortex/releases/latest/download/gortex_linux_${ARCH}.rpm"
sudo rpm -ivh "gortex_linux_${ARCH}.rpm"
```

## Linux — Alpine (.apk)

```bash
ARCH=$(uname -m); [ "$ARCH" = x86_64 ] && ARCH=amd64; [ "$ARCH" = aarch64 ] && ARCH=arm64
curl -LO "https://github.com/zzet/gortex/releases/latest/download/gortex_linux_${ARCH}.apk"
sudo apk add --allow-untrusted "gortex_linux_${ARCH}.apk"
```

## Direct binary download (any Linux or macOS)

```bash
# Pick the right asset for your OS/arch
OS=$(uname -s | tr '[:upper:]' '[:lower:]')  # linux or darwin
ARCH=$(uname -m)
case $ARCH in
  x86_64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
esac

curl -LO "https://github.com/zzet/gortex/releases/latest/download/gortex_${OS}_${ARCH}.tar.gz"
tar -xzf "gortex_${OS}_${ARCH}.tar.gz"
sudo mv gortex /usr/local/bin/
```

On macOS, if you downloaded via browser (not `curl`), remove the quarantine flag once:

```bash
xattr -d com.apple.quarantine /usr/local/bin/gortex
```

## Verify the install

```bash
gortex version
```

## Verifying releases (supply-chain security)

Every GitHub release is:

- **Signed with [cosign](https://github.com/sigstore/cosign)** — keyless via GitHub's OIDC identity. Each artifact ships with matching `.sig` and `.pem` files that cryptographically prove the binary came from this repo's release workflow.
- **Attested with [SLSA-3 provenance](https://slsa.dev/spec/v1.0/levels#build-l3)** — a `multiple.intoto.jsonl` file attached to each release records the exact commit, builder, and workflow that produced every artifact. Tamper-evident and non-forgeable.
- **Scanned against ~72 AV engines via [VirusTotal](https://virustotal.com)** — the detection count (e.g. `0 / 72`) is posted in each release's notes, with a link to the full per-engine report.

You don't need to verify manually if you're installing via `brew` / `dpkg` / `rpm` — those paths go through package managers that check integrity themselves. Verification matters when you're redistributing Gortex downstream, running it inside a locked-down enterprise environment, or writing your own installer.

**cosign** — install once via `brew install cosign`, `apt install cosign`, or from [the cosign releases page](https://github.com/sigstore/cosign/releases). Then:

```bash
TAG=v0.14.0                          # replace with the release you downloaded
FILE=gortex_linux_amd64.tar.gz       # pick your artifact

BASE="https://github.com/zzet/gortex/releases/download/${TAG}"
curl -LO "${BASE}/${FILE}"
curl -LO "${BASE}/${FILE}.sig"
curl -LO "${BASE}/${FILE}.pem"

cosign verify-blob \
  --certificate "${FILE}.pem" \
  --signature "${FILE}.sig" \
  --certificate-identity-regexp 'https://github\.com/zzet/gortex/\.github/workflows/.+@refs/tags/v.+' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  "${FILE}"
```

Expected output: `Verified OK`. Anything else — stop and delete the binary.

**SLSA-3** — install [`slsa-verifier`](https://github.com/slsa-framework/slsa-verifier/releases) once. Then:

```bash
curl -LO "${BASE}/multiple.intoto.jsonl"

slsa-verifier verify-artifact "${FILE}" \
  --provenance-path multiple.intoto.jsonl \
  --source-uri github.com/zzet/gortex \
  --source-tag "${TAG}"
```

Expected output ends with `PASSED: SLSA verification passed`.

**VirusTotal** — open the release page on GitHub. The notes include a per-asset scan table like `gortex_linux_amd64.tar.gz — 0 / 72` with a link to the full report. A non-zero detection on a Go binary is usually a false positive (Go's static linking + stripped symbols trips heuristics), but you should still compare against prior releases before trusting the download.

## From source

Requires Go 1.25+ and a C toolchain (the tree-sitter extractors are CGO — no way around it).

```bash
git clone https://github.com/zzet/gortex.git
cd gortex
go build -o gortex ./cmd/gortex/
sudo mv gortex /usr/local/bin/
```

Or without cloning:

```bash
go install github.com/zzet/gortex/cmd/gortex@latest
```

`go install` drops the binary into `$(go env GOBIN)` (default `~/go/bin`) — make sure that's on your `PATH`.
