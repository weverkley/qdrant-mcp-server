#!/bin/sh
set -e

# Repository settings
GITHUB_REPO="weverkley/qdrant-mcp-server"
BINARY_NAME="qdrant-mcp-server"

# Color support detection
if [ -t 1 ]; then
    RED="\033[1;31m"
    GREEN="\033[1;32m"
    YELLOW="\033[1;33m"
    BLUE="\033[1;34m"
    NC="\033[0m"
else
    RED=""
    GREEN=""
    YELLOW=""
    BLUE=""
    NC=""
fi

log_info() {
    printf "${BLUE}info:${NC} %s\n" "$1"
}

log_success() {
    printf "${GREEN}success:${NC} %s\n" "$1"
}

log_warning() {
    printf "${YELLOW}warning:${NC} %s\n" "$1"
}

log_error() {
    printf "${RED}error:${NC} %s\n" "$1" >&2
}

# 1. Platform Detection
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"

case "$OS" in
    darwin)
        OS_TARGET="darwin"
        ;;
    linux)
        OS_TARGET="linux"
        ;;
    msys*|cygwin*|mingw*|nt)
        OS_TARGET="windows"
        ;;
    *)
        log_error "Unsupported Operating System: $OS"
        exit 1
        ;;
esac

case "$ARCH" in
    x86_64|amd64)
        ARCH_TARGET="amd64"
        ;;
    arm64|aarch64)
        ARCH_TARGET="arm64"
        ;;
    *)
        log_error "Unsupported CPU Architecture: $ARCH"
        exit 1
        ;;
esac

# 2. Dependency Check
if ! command -v curl >/dev/null 2>&1 && ! command -v wget >/dev/null 2>&1; then
    log_error "Either 'curl' or 'wget' is required to install ${BINARY_NAME}."
    exit 1
fi

if [ "$OS_TARGET" = "windows" ]; then
    if ! command -v unzip >/dev/null 2>&1; then
        log_error "'unzip' is required to extract the Windows package."
        exit 1
    fi
else
    if ! command -v tar >/dev/null 2>&1; then
        log_error "'tar' is required to extract the package."
        exit 1
    fi
fi

# 3. Determine Release Version
if [ -z "$VERSION" ]; then
    log_info "Fetching the latest release version..."
    
    API_URL="https://api.github.com/repos/${GITHUB_REPO}/releases/latest"
    AUTH_HEADER=""
    
    if [ -n "$GITHUB_TOKEN" ]; then
        AUTH_HEADER="Authorization: token $GITHUB_TOKEN"
    elif [ -n "$GH_TOKEN" ]; then
        AUTH_HEADER="Authorization: token $GH_TOKEN"
    fi

    # Fetch using curl or wget
    if command -v curl >/dev/null 2>&1; then
        if [ -n "$AUTH_HEADER" ]; then
            RESPONSE=$(curl -sS -H "$AUTH_HEADER" "$API_URL" 2>/dev/null || true)
        else
            RESPONSE=$(curl -sS "$API_URL" 2>/dev/null || true)
        fi
    else
        if [ -n "$AUTH_HEADER" ]; then
            RESPONSE=$(wget -qO- --header="$AUTH_HEADER" "$API_URL" 2>/dev/null || true)
        else
            RESPONSE=$(wget -qO- "$API_URL" 2>/dev/null || true)
        fi
    fi

    # Extract tag_name using standard grep and sed
    VERSION=$(echo "$RESPONSE" | grep '"tag_name":' | sed -E 's/.*"tag_name":\s*"([^"]+)".*/\1/' | tr -d '[:space:]' 2>/dev/null || true)

    # Fallback to redirect URL method if API method fails
    if [ -z "$VERSION" ] || [ "$VERSION" = "latest" ] || echo "$RESPONSE" | grep -q "message" 2>/dev/null; then
        log_warning "GitHub API rate limit reached or private repository auth required. Retrying with redirect URL method..."
        if command -v curl >/dev/null 2>&1; then
            LATEST_URL=$(curl -sSL -o /dev/null -w "%{url_effective}" "https://github.com/${GITHUB_REPO}/releases/latest")
            VERSION="${LATEST_URL##*/}"
        else
            LATEST_URL=$(wget --max-redirect=0 "https://github.com/${GITHUB_REPO}/releases/latest" 2>&1 | grep "Location:" | awk '{print $2}')
            VERSION="${LATEST_URL##*/}"
        fi
    fi
    
    if [ -z "$VERSION" ] || [ "$VERSION" = "latest" ]; then
        log_error "Failed to retrieve the latest version."
        log_error "If this is a private repository, please pass GITHUB_TOKEN or specify the version manually, e.g.:"
        log_error "  curl -fsSL https://raw.githubusercontent.com/... | VERSION=1.4.0 sh"
        log_error "  Or with authentication:"
        log_error "  curl -fsSL -H 'Authorization: token YOUR_TOKEN' https://raw.githubusercontent.com/... | GITHUB_TOKEN=YOUR_TOKEN sh"
        exit 1
    fi
fi

# Normalize version tag to always include the 'v' prefix
case "$VERSION" in
    v*) ;;
    *) VERSION="v$VERSION" ;;
esac

log_info "Selected release version: ${VERSION}"

# 4. Construct Download URL and Filename
if [ "$OS_TARGET" = "windows" ]; then
    FILE_EXT="zip"
    BINARY_FILE="${BINARY_NAME}.exe"
else
    FILE_EXT="tar.gz"
    BINARY_FILE="${BINARY_NAME}"
fi

ASSET_NAME="${BINARY_NAME}-${VERSION}-${OS_TARGET}-${ARCH_TARGET}.${FILE_EXT}"
DOWNLOAD_URL="https://github.com/${GITHUB_REPO}/releases/download/${VERSION}/${ASSET_NAME}"

# 5. Create Temporary Directory for Extraction
TMP_DIR=$(mktemp -d)
clean_up() {
    rm -rf "$TMP_DIR"
}
trap clean_up EXIT

# 6. Download Archive
log_info "Downloading ${DOWNLOAD_URL}..."
if command -v curl >/dev/null 2>&1; then
    curl -fL -o "${TMP_DIR}/${ASSET_NAME}" "$DOWNLOAD_URL"
else
    wget -O "${TMP_DIR}/${ASSET_NAME}" "$DOWNLOAD_URL"
fi

if [ ! -f "${TMP_DIR}/${ASSET_NAME}" ]; then
    log_error "Failed to download release asset."
    exit 1
fi

# 7. Extract Archive
log_info "Extracting archive..."
cd "$TMP_DIR"
if [ "$OS_TARGET" = "windows" ]; then
    unzip -q "$ASSET_NAME"
else
    tar -xzf "$ASSET_NAME"
fi

if [ ! -f "$BINARY_FILE" ]; then
    log_error "Binary '${BINARY_FILE}' not found inside the downloaded archive."
    exit 1
fi
chmod +x "$BINARY_FILE"

# 8. Installation Location Decision
INSTALL_DIR="/usr/local/bin"

if [ -w "$INSTALL_DIR" ]; then
    log_info "Installing to ${INSTALL_DIR}/${BINARY_FILE}..."
    mv "$BINARY_FILE" "${INSTALL_DIR}/${BINARY_FILE}"
else
    INSTALL_DIR="$HOME/.local/bin"
    log_warning "No write permission for /usr/local/bin. Falling back to ${INSTALL_DIR}."
    
    mkdir -p "$INSTALL_DIR"
    mv "$BINARY_FILE" "${INSTALL_DIR}/${BINARY_FILE}"
    
    case ":$PATH:" in
        *:"$INSTALL_DIR":*) ;;
        *)
            ADDED_TO_PROFILE=false
            
            append_to_file() {
                local file="$1"
                local line="export PATH=\"\$PATH:${INSTALL_DIR}\""
                if [ -f "$file" ]; then
                    if ! grep -F -q "$line" "$file" 2>/dev/null; then
                        echo "" >> "$file"
                        echo "$line" >> "$file"
                        log_info "Added ${INSTALL_DIR} to ${file}"
                        ADDED_TO_PROFILE=true
                    fi
                fi
            }

            append_to_file "$HOME/.bashrc"
            append_to_file "$HOME/.zshrc"
            append_to_file "$HOME/.profile"
            append_to_file "$HOME/.bash_profile"

            if [ "$ADDED_TO_PROFILE" = true ]; then
                log_success "Successfully appended ${INSTALL_DIR} to your shell path configuration!"
                log_warning "Please run 'source ~/.bashrc' (or your shell's config file) or restart your terminal to apply."
            else
                log_warning "The directory ${INSTALL_DIR} is not in your PATH."
                log_warning "To run it directly, add this to your shell profile (~/.bashrc, ~/.zshrc, etc.):"
                echo "  export PATH=\"\$PATH:${INSTALL_DIR}\""
            fi
            ;;
    esac
fi

log_success "${BINARY_NAME} ${VERSION} installed successfully at ${INSTALL_DIR}/${BINARY_FILE}!"
echo ""
echo "🚀 Exposing Smart CLI Capabilities:"
echo "  - Ingest your codebase manually:     ${BINARY_NAME} ingest"
echo "  - List configured agent skills:      ${BINARY_NAME} list-skills"
echo "  - Override settings dynamically:     ${BINARY_NAME} ingest --collection my-custom-collection"
echo ""
echo "💡 Auto-Discovery Active:"
echo "  The CLI will automatically read environment variables from your agent's"
echo "  settings files (.mcp.json, settings.local.json, config.toml) if present in the"
echo "  directory tree, so you don't need to manually configure them in your terminal!"
echo ""
