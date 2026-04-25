#!/bin/sh
# Gortex one-line installer for macOS and Linux.
#
# Usage:
#   curl -fsSL https://get.gortex.dev | sh
#   curl -fsSL https://raw.githubusercontent.com/zzet/gortex/main/scripts/install.sh | sh
#
# Configuration via environment variables (all optional):
#   GORTEX_VERSION        Release tag to install ("latest" or "v0.15.0")
#   GORTEX_INSTALL_DIR    Install directory (default: $HOME/.local/bin)
#   GORTEX_NO_VERIFY      Set to skip checksum + cosign verification
#   GORTEX_NO_PATH        Set to skip PATH update in shell rc
#   GORTEX_NO_BREW        Set to skip Homebrew (macOS) and force the tarball path
#   GORTEX_FORCE          Set to overwrite an existing binary without backup
#   GORTEX_DOWNLOAD_BASE  Override release download base URL (for testing)

set -eu

GORTEX_REPO="zzet/gortex"
GORTEX_BIN="gortex"
DOWNLOAD_BASE="${GORTEX_DOWNLOAD_BASE:-https://github.com/${GORTEX_REPO}/releases}"

if [ -t 1 ] && command -v tput >/dev/null 2>&1 && [ "$(tput colors 2>/dev/null || echo 0)" -ge 8 ]; then
	C_BOLD="$(tput bold)"
	C_DIM="$(tput dim 2>/dev/null || printf '')"
	C_RED="$(tput setaf 1)"
	C_GREEN="$(tput setaf 2)"
	C_YELLOW="$(tput setaf 3)"
	C_BLUE="$(tput setaf 4)"
	C_RESET="$(tput sgr0)"
else
	C_BOLD=""; C_DIM=""; C_RED=""; C_GREEN=""; C_YELLOW=""; C_BLUE=""; C_RESET=""
fi

info() { printf '%s==>%s %s\n' "${C_BLUE}${C_BOLD}" "${C_RESET}" "$1"; }
ok()   { printf '%s ok%s  %s\n' "${C_GREEN}${C_BOLD}" "${C_RESET}" "$1"; }
warn() { printf '%s  !%s  %s\n' "${C_YELLOW}${C_BOLD}" "${C_RESET}" "$1" >&2; }
die()  { printf '%s  x%s  %s\n' "${C_RED}${C_BOLD}" "${C_RESET}" "$1" >&2; exit 1; }

# Pick a downloader once.
DOWNLOADER=""
if command -v curl >/dev/null 2>&1; then
	DOWNLOADER="curl"
elif command -v wget >/dev/null 2>&1; then
	DOWNLOADER="wget"
else
	die "neither curl nor wget is installed; install one and retry"
fi

http_get() {
	# $1=url, $2=dest. Returns non-zero on failure (callers decide whether fatal).
	case "$DOWNLOADER" in
		curl) curl --fail --silent --show-error --location --output "$2" "$1" ;;
		wget) wget --quiet --output-document="$2" "$1" ;;
	esac
}

detect_os() {
	uname_s="$(uname -s)"
	case "$uname_s" in
		Linux) echo linux ;;
		Darwin) echo darwin ;;
		# Catch users running this in WSL — Windows binaries aren't published yet,
		# but the Linux build runs fine under WSL.
		MINGW*|MSYS*|CYGWIN*) die "Windows native install isn't supported yet; run this from WSL or use the manual download from https://github.com/${GORTEX_REPO}/releases" ;;
		*) die "unsupported OS: $uname_s (Linux and macOS supported)" ;;
	esac
}

detect_arch() {
	uname_m="$(uname -m)"
	case "$uname_m" in
		x86_64|amd64) echo amd64 ;;
		aarch64|arm64) echo arm64 ;;
		*) die "unsupported architecture: $uname_m (amd64 and arm64 supported)" ;;
	esac
}

verify_sha256() {
	# $1=file, $2=expected_hex
	if command -v sha256sum >/dev/null 2>&1; then
		actual="$(sha256sum "$1" | awk '{print $1}')"
	elif command -v shasum >/dev/null 2>&1; then
		actual="$(shasum -a 256 "$1" | awk '{print $1}')"
	else
		warn "no sha256sum or shasum available; cannot verify checksum"
		return 0
	fi
	if [ "$actual" != "$2" ]; then
		die "checksum mismatch on $(basename "$1")
  expected: $2
  actual:   $actual"
	fi
	ok "sha256 verified ($(basename "$1"))"
}

# Append a PATH export to the user's shell rc file if it isn't already there.
# Uses a marker block so re-runs are idempotent.
update_path_in_rc() {
	install_dir="$1"
	marker_begin="# >>> gortex installer >>>"
	marker_end="# <<< gortex installer <<<"

	case ":${PATH}:" in
		*":${install_dir}:"*)
			ok "$install_dir already on PATH"
			return 0
			;;
	esac

	user_shell="$(basename "${SHELL:-sh}")"
	rcfiles=""
	case "$user_shell" in
		bash)
			[ -f "$HOME/.bashrc" ] && rcfiles="$HOME/.bashrc"
			# macOS terminals start a login bash, which reads .bash_profile.
			if [ "$(uname -s)" = Darwin ] && [ -f "$HOME/.bash_profile" ]; then
				rcfiles="${rcfiles:+$rcfiles }$HOME/.bash_profile"
			fi
			[ -z "$rcfiles" ] && rcfiles="$HOME/.bashrc"
			;;
		zsh)
			rcfiles="${ZDOTDIR:-$HOME}/.zshrc"
			;;
		fish)
			rcfiles="$HOME/.config/fish/config.fish"
			;;
		*)
			rcfiles="$HOME/.profile"
			;;
	esac

	for rc in $rcfiles; do
		[ -n "$rc" ] || continue
		if [ -f "$rc" ] && grep -qF "$marker_begin" "$rc" 2>/dev/null; then
			ok "PATH block already present in $(printf '%s' "$rc" | sed "s|^$HOME|~|")"
			continue
		fi
		mkdir -p "$(dirname "$rc")"
		if [ "$user_shell" = fish ]; then
			{
				printf '\n%s\n' "$marker_begin"
				printf 'fish_add_path -aP %s\n' "$install_dir"
				printf '%s\n' "$marker_end"
			} >> "$rc"
		else
			{
				printf '\n%s\n' "$marker_begin"
				# We want a literal $PATH in the rc file so the user's shell
				# expands it at source time, not us at install time.
				# shellcheck disable=SC2016
				printf 'export PATH="%s:$PATH"\n' "$install_dir"
				printf '%s\n' "$marker_end"
			} >> "$rc"
		fi
		ok "added $install_dir to PATH in $(printf '%s' "$rc" | sed "s|^$HOME|~|")"
	done
}

# Homebrew handoff. Cask is macOS-only and brew handles its own PATH, so we
# only divert when the user has plain `brew` on PATH on macOS, isn't pinning
# a version, and didn't pick a custom install dir.
should_use_brew() {
	[ "$1" = darwin ] || return 1
	[ -z "${GORTEX_NO_BREW:-}" ] || return 1
	[ "${GORTEX_VERSION:-latest}" = latest ] || return 1
	[ -z "${GORTEX_INSTALL_DIR:-}" ] || return 1
	command -v brew >/dev/null 2>&1
}

install_via_brew() {
	if brew list --cask gortex >/dev/null 2>&1; then
		info "gortex cask already installed; upgrading via Homebrew"
		brew upgrade --cask gortex || die "brew upgrade failed"
	else
		info "installing via Homebrew (zzet/tap/gortex)"
		brew install zzet/tap/gortex || die "brew install failed"
	fi

	bin="$(command -v gortex || true)"
	[ -n "$bin" ] || die "brew finished but 'gortex' is not on PATH"
	ok "installed $bin"

	# Best-effort daemon restart, mirroring the tarball path.
	if "$bin" daemon status >/dev/null 2>&1; then
		info "restarting running daemon onto new binary"
		"$bin" daemon restart >/dev/null 2>&1 || warn "daemon restart failed; run 'gortex daemon restart' manually"
	fi

	if version_out="$("$bin" version 2>/dev/null)"; then
		ok "$version_out"
	fi

	printf '\n%sNext steps:%s\n' "${C_BOLD}" "${C_RESET}"
	printf '  - %sgortex install%s   one-time machine setup (MCP, skills, slash commands)\n' "${C_BOLD}" "${C_RESET}"
	printf '  - %sgortex init%s      run inside a repo to wire up your AI assistant\n' "${C_BOLD}" "${C_RESET}"
	printf '\nDocs: https://github.com/%s\n\n' "$GORTEX_REPO"
}

main() {
	os="$(detect_os)"
	arch="$(detect_arch)"
	version="${GORTEX_VERSION:-latest}"

	if should_use_brew "$os"; then
		printf '\n%sGortex installer%s\n' "${C_BOLD}" "${C_RESET}"
		printf '  os:      %s\n' "$os"
		printf '  arch:    %s\n' "$arch"
		printf '  version: %s (via Homebrew — set GORTEX_NO_BREW=1 to use the tarball)\n\n' "$version"
		install_via_brew
		return 0
	fi

	install_dir="${GORTEX_INSTALL_DIR:-$HOME/.local/bin}"

	printf '\n%sGortex installer%s\n' "${C_BOLD}" "${C_RESET}"
	printf '  os:      %s\n' "$os"
	printf '  arch:    %s\n' "$arch"
	printf '  version: %s\n' "$version"
	printf '  target:  %s/%s\n\n' "$install_dir" "$GORTEX_BIN"

	asset="gortex_${os}_${arch}.tar.gz"
	if [ "$version" = latest ]; then
		base_url="${DOWNLOAD_BASE}/latest/download"
	else
		case "$version" in
			v*) tag="$version" ;;
			*)  tag="v$version" ;;
		esac
		base_url="${DOWNLOAD_BASE}/download/${tag}"
	fi
	asset_url="${base_url}/${asset}"
	checksums_url="${base_url}/checksums.txt"

	tmpdir="$(mktemp -d 2>/dev/null || mktemp -d -t gortex-install)"
	# shellcheck disable=SC2064  # we want $tmpdir expanded now, not at trap time.
	trap "rm -rf '$tmpdir'" EXIT INT TERM

	info "downloading $asset"
	http_get "$asset_url" "$tmpdir/$asset" \
		|| die "download failed: $asset_url"

	if [ -z "${GORTEX_NO_VERIFY:-}" ]; then
		info "downloading checksums.txt"
		if http_get "$checksums_url" "$tmpdir/checksums.txt"; then
			# checksums.txt is `<sha256>  <filename>` per goreleaser default.
			# Some versions prefix the filename with `*` for binary mode.
			expected="$(awk -v f="$asset" '$2==f || $2=="*"f {print $1; exit}' "$tmpdir/checksums.txt")"
			if [ -z "$expected" ]; then
				warn "checksums.txt did not contain $asset; skipping verification"
			else
				verify_sha256 "$tmpdir/$asset" "$expected"
			fi
		else
			warn "could not fetch checksums.txt; skipping verification"
		fi

		# Optional cosign verification — only if cosign happens to be installed.
		# Releases ship .sig + .pem bound to GitHub Actions OIDC identity; see
		# README "Verifying releases" for the full standalone command.
		if command -v cosign >/dev/null 2>&1; then
			info "verifying cosign signature"
			if http_get "${asset_url}.sig" "$tmpdir/$asset.sig" \
				&& http_get "${asset_url}.pem" "$tmpdir/$asset.pem"; then
				if cosign verify-blob \
						--certificate "$tmpdir/$asset.pem" \
						--signature "$tmpdir/$asset.sig" \
						--certificate-identity-regexp 'https://github\.com/zzet/gortex/.*' \
						--certificate-oidc-issuer https://token.actions.githubusercontent.com \
						"$tmpdir/$asset" >/dev/null 2>&1; then
					ok "cosign verified"
				else
					die "cosign signature verification failed (set GORTEX_NO_VERIFY=1 to skip)"
				fi
			else
				warn "could not fetch cosign signature; skipping"
			fi
		fi
	else
		warn "verification disabled (GORTEX_NO_VERIFY)"
	fi

	info "extracting"
	( cd "$tmpdir" && tar -xzf "$asset" )
	[ -f "$tmpdir/$GORTEX_BIN" ] || die "archive did not contain a $GORTEX_BIN binary"
	chmod +x "$tmpdir/$GORTEX_BIN"

	mkdir -p "$install_dir" || die "cannot create $install_dir"
	# Probe writability before we trash a partial install.
	if ! ( : > "$install_dir/.gortex-write-test" ) 2>/dev/null; then
		die "no write permission to $install_dir
  Set GORTEX_INSTALL_DIR=\$HOME/.local/bin, or rerun with sudo if you really want a system path."
	fi
	rm -f "$install_dir/.gortex-write-test"

	if [ -e "$install_dir/$GORTEX_BIN" ] && [ -z "${GORTEX_FORCE:-}" ]; then
		backup="$install_dir/${GORTEX_BIN}.previous"
		info "backing up existing binary to $backup"
		mv -f "$install_dir/$GORTEX_BIN" "$backup"
	fi
	mv -f "$tmpdir/$GORTEX_BIN" "$install_dir/$GORTEX_BIN"
	ok "installed $install_dir/$GORTEX_BIN"

	# macOS Gatekeeper sets com.apple.quarantine on browser downloads. curl/wget
	# don't, but a re-run might inherit it from a prior browser fetch — strip
	# unconditionally; failure is harmless.
	if [ "$os" = darwin ] && command -v xattr >/dev/null 2>&1; then
		xattr -d com.apple.quarantine "$install_dir/$GORTEX_BIN" 2>/dev/null || true
	fi

	if [ -z "${GORTEX_NO_PATH:-}" ]; then
		update_path_in_rc "$install_dir"
	fi

	# If the daemon is running an older binary, restart it on the new one.
	# Best-effort: never block install on this.
	if "$install_dir/$GORTEX_BIN" daemon status >/dev/null 2>&1; then
		info "restarting running daemon onto new binary"
		"$install_dir/$GORTEX_BIN" daemon restart >/dev/null 2>&1 || warn "daemon restart failed; run 'gortex daemon restart' manually"
	fi

	# Print version banner. The binary lives at an absolute path so we don't
	# depend on PATH being refreshed in the current shell.
	if version_out="$("$install_dir/$GORTEX_BIN" version 2>/dev/null)"; then
		ok "$version_out"
	fi

	printf '\n%sNext steps:%s\n' "${C_BOLD}" "${C_RESET}"
	case ":${PATH}:" in
		*":${install_dir}:"*) ;;
		*)
			printf '  1. Open a new shell (or %ssource%s your rc file) so PATH picks up %s\n' "${C_DIM}" "${C_RESET}" "$install_dir"
			;;
	esac
	printf '  - %sgortex install%s   one-time machine setup (MCP, skills, slash commands)\n' "${C_BOLD}" "${C_RESET}"
	printf '  - %sgortex init%s      run inside a repo to wire up your AI assistant\n' "${C_BOLD}" "${C_RESET}"
	printf '\nDocs: https://github.com/%s\n\n' "$GORTEX_REPO"
}

main "$@"
