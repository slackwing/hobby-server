#!/usr/bin/env bash
# rv-server installation script. Mirrors the install pattern of
# github.com/slackwing/manuscript-studio (Docker + Liquibase + systemd-via-Docker).
#
# One-liner:
#   bash <(curl -sSL -H "Cache-Control: no-cache" \
#     https://raw.githubusercontent.com/slackwing/rv-server/main/install.sh)
#
# SCRIPT_VERSION: bump on EVERY change to this file.
# Format: YYYY-MM-DD.N (N increments within the same day).
SCRIPT_VERSION="2026-06-21.1"

set -euo pipefail

CONFIG_DIR="$HOME/.config/rv-server"
CONFIG_FILE="$CONFIG_DIR/config.yaml"
CONFIG_SOURCE_TEMPLATE="config.example.yaml"
REPO_URL="https://github.com/slackwing/rv-server"
CONTAINER_NAME="rv-server"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'
log_info() { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; exit 1; }
log_step() { echo -e "${BLUE}[STEP]${NC} $1"; }

mkdir -p "$CONFIG_DIR/logs"
INSTALL_LOG="$CONFIG_DIR/logs/install.log"
{
    echo ""
    echo "================================================================"
    echo "rv-server install run starting: $(date -u +'%Y-%m-%dT%H:%M:%SZ')"
    echo "Script version: $SCRIPT_VERSION"
    echo "User: $(whoami)@$(hostname)"
    echo "================================================================"
} >> "$INSTALL_LOG"
exec > >(tee -a "$INSTALL_LOG") 2>&1
trap 'echo "Install finished: $(date -u +%Y-%m-%dT%H:%M:%SZ) exit=$?" >> "$INSTALL_LOG"' EXIT

echo "========================================="
echo "   rv-server installation"
echo "   Script version: $SCRIPT_VERSION"
echo "========================================="
log_info "Config file: $CONFIG_FILE"

# ----- Step 1: Config -----
log_step "Checking config..."
if [[ ! -f "$CONFIG_FILE" ]]; then
    log_info "Downloading config template..."
    mkdir -p "$CONFIG_DIR"
    curl -sSL "$REPO_URL/raw/main/$CONFIG_SOURCE_TEMPLATE" -o "$CONFIG_FILE" || {
        log_error "Failed to download $CONFIG_SOURCE_TEMPLATE — check repo URL or your network."
    }
    log_warn "Config template written to $CONFIG_FILE"
    echo ""
    echo "Edit the file to set:"
    echo "  - database.password (the rv_user Postgres password)"
    echo "  - server.env: production (default) or development"
    echo ""
    echo "Then re-run this script."
    exit 0
fi
log_info "Config found"

# ----- Step 2: Dependencies -----
log_step "Checking dependencies..."
check_dep() {
    command -v "$1" &>/dev/null || log_error "$1 not installed"
    log_info "✓ $1 found"
}
check_dep docker
check_dep psql
check_dep git

# ----- Step 3: Parse config -----
log_step "Parsing config..."
get_config() {
    grep "^[[:space:]]*$1:" "$CONFIG_FILE" | head -1 | sed "s/.*$1:[[:space:]]*[\"']*\([^\"']*\)[\"']*/\1/"
}
DB_HOST=$(get_config "host")
DB_PORT=$(get_config "port")
DB_NAME=$(get_config "name")
DB_USER=$(get_config "user")
DB_PASSWORD=$(get_config "password")
SERVER_PORT=$(get_config "port" | head -1)  # first `port:` is database; we need server port
# Better: extract specifically from server: block
SERVER_PORT=$(awk '/^server:/{flag=1;next} flag && /^[a-z]/{flag=0} flag && /port:/{print $2; exit}' "$CONFIG_FILE" | tr -d '"')
[[ -z "$SERVER_PORT" ]] && SERVER_PORT=5002

log_info "Database: $DB_USER@$DB_HOST:$DB_PORT/$DB_NAME"
log_info "Server port: $SERVER_PORT"

# ----- Step 4: Database connectivity -----
log_step "Testing database connection..."
PGPASSWORD="$DB_PASSWORD" psql -h "$DB_HOST" -p "$DB_PORT" -U "$DB_USER" -d "$DB_NAME" -c "SELECT 1;" >/dev/null 2>&1 || {
    log_error "Cannot connect to database. Verify Cloud SQL Auth Proxy is running and database exists."
}
log_info "Database OK"

# ----- Step 5: Clone or pull this repo -----
log_step "Fetching latest source..."
SRC_DIR="$CONFIG_DIR/src"
if [[ -d "$SRC_DIR/.git" ]]; then
    git -C "$SRC_DIR" fetch --quiet && git -C "$SRC_DIR" reset --hard origin/main --quiet
    log_info "Updated $SRC_DIR"
else
    rm -rf "$SRC_DIR"
    git clone --quiet "$REPO_URL.git" "$SRC_DIR"
    log_info "Cloned to $SRC_DIR"
fi

# ----- Step 6: Build images -----
log_step "Building rv-server image..."
docker build -q -t rv-server:latest "$SRC_DIR" >/dev/null
log_info "✓ rv-server image"

log_step "Building rv-server-liquibase image..."
docker build -q -f "$SRC_DIR/Dockerfile.liquibase" -t rv-server-liquibase:latest "$SRC_DIR" >/dev/null
log_info "✓ liquibase image"

# ----- Step 7: Run migrations -----
log_step "Running Liquibase migrations..."
docker run --rm \
    --network host \
    rv-server-liquibase:latest \
    --changeLogFile=changelog/db.changelog-master.xml \
    --url="jdbc:postgresql://$DB_HOST:$DB_PORT/$DB_NAME" \
    --username="$DB_USER" \
    --password="$DB_PASSWORD" \
    update || log_warn "Migrations exit nonzero (already up-to-date is fine)"

# ----- Step 8: Restart server container -----
log_step "Starting rv-server..."
if docker ps -a --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
    docker stop "$CONTAINER_NAME" >/dev/null 2>&1 || true
    docker rm "$CONTAINER_NAME" >/dev/null 2>&1 || true
fi

docker run -d \
    --name "$CONTAINER_NAME" \
    --restart unless-stopped \
    --network host \
    -v "$CONFIG_FILE:/config/config.yaml:ro" \
    rv-server:latest \
    rv-server --config /config/config.yaml || log_error "Failed to start container"

sleep 2
if docker ps --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
    log_info "✓ rv-server running on :$SERVER_PORT"
else
    log_error "Container failed to start. Check: docker logs $CONTAINER_NAME"
fi

echo ""
log_info "Installation complete."
echo ""
echo "Next steps:"
echo "  1. Add a user:"
echo "     docker run --rm --network host -v \"$CONFIG_FILE:/config/config.yaml:ro\" rv-server:latest add-user --config /config/config.yaml <username> <password>"
echo "  2. Verify Apache proxies /rv/api/* to 127.0.0.1:$SERVER_PORT"
echo "  3. Test: curl -sS http://127.0.0.1:$SERVER_PORT/healthz"
echo ""
