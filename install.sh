#!/usr/bin/env bash
# hobby-server installation script. Mirrors the install pattern of
# github.com/slackwing/manuscript-studio (Docker + Liquibase +
# systemd-via-Docker).
#
# Multi-project: one binary, one container, but each configured project
# in ~/.config/hobby-server/config.yaml has its own database and runs
# its own Liquibase migrations against it.
#
# One-liner:
#   bash <(curl -sSL -H "Cache-Control: no-cache" \
#     https://raw.githubusercontent.com/slackwing/hobby-server/main/install.sh)
#
# SCRIPT_VERSION: bump on EVERY change to this file.
# Format: YYYY-MM-DD.N (N increments within the same day).
SCRIPT_VERSION="2026-06-21.2"

set -euo pipefail

CONFIG_DIR="$HOME/.config/hobby-server"
CONFIG_FILE="$CONFIG_DIR/config.yaml"
CONFIG_SOURCE_TEMPLATE="config.example.yaml"
REPO_URL="https://github.com/slackwing/hobby-server"
CONTAINER_NAME="hobby-server"

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
    echo "hobby-server install run starting: $(date -u +'%Y-%m-%dT%H:%M:%SZ')"
    echo "Script version: $SCRIPT_VERSION"
    echo "User: $(whoami)@$(hostname)"
    echo "================================================================"
} >> "$INSTALL_LOG"
exec > >(tee -a "$INSTALL_LOG") 2>&1
trap 'echo "Install finished: $(date -u +%Y-%m-%dT%H:%M:%SZ) exit=$?" >> "$INSTALL_LOG"' EXIT

echo "========================================="
echo "   hobby-server installation"
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
    echo "  - projects[].database.password (per project)"
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
check_dep python3
# python3 must have yaml available so we can parse projects[]. Don't proceed
# without it — bash YAML parsing is fragile, especially with nested lists.
python3 -c 'import yaml' 2>/dev/null || log_error "python3-yaml (PyYAML) not installed. Install with: apt install python3-yaml  OR  pip install pyyaml"
log_info "✓ python3+yaml"

# ----- Step 3: Parse config -----
log_step "Parsing config..."
SERVER_PORT=$(python3 -c "import yaml,sys; c=yaml.safe_load(open('$CONFIG_FILE')); print(c.get('server',{}).get('port',5002))")
log_info "Server port: $SERVER_PORT"

# Project names, newline-separated.
PROJECT_NAMES=$(python3 -c "import yaml; print('\n'.join(p['name'] for p in yaml.safe_load(open('$CONFIG_FILE')).get('projects',[])))")
if [[ -z "$PROJECT_NAMES" ]]; then
    log_error "No projects[] in config. Add at least one."
fi
log_info "Projects: $(echo $PROJECT_NAMES | tr '\n' ' ')"

# For one project, dump host/port/name/user/password to stdout (tab-separated).
project_db_info() {
    local name="$1"
    python3 -c "
import yaml, sys
c = yaml.safe_load(open('$CONFIG_FILE'))
for p in c.get('projects', []):
    if p['name'] == '$name':
        d = p['database']
        print('\t'.join([d['host'], str(d['port']), d['name'], d['user'], d['password']]))
        sys.exit(0)
sys.exit('project not found: $name')
"
}

# ----- Step 4: Database connectivity (per project) -----
log_step "Testing database connections..."
for name in $PROJECT_NAMES; do
    IFS=$'\t' read -r DB_HOST DB_PORT DB_NAME DB_USER DB_PASSWORD <<< "$(project_db_info "$name")"
    PGPASSWORD="$DB_PASSWORD" psql -h "$DB_HOST" -p "$DB_PORT" -U "$DB_USER" -d "$DB_NAME" -c "SELECT 1;" >/dev/null 2>&1 || {
        log_error "Cannot connect to project '$name' DB: $DB_USER@$DB_HOST:$DB_PORT/$DB_NAME"
    }
    log_info "  ✓ $name: $DB_USER@$DB_HOST:$DB_PORT/$DB_NAME"
done

# ----- Step 5: Fetch source -----
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
log_step "Building hobby-server image..."
docker build -q -t hobby-server:latest "$SRC_DIR" >/dev/null
log_info "✓ hobby-server image"

log_step "Building hobby-server-liquibase image..."
docker build -q -f "$SRC_DIR/Dockerfile.liquibase" -t hobby-server-liquibase:latest "$SRC_DIR" >/dev/null
log_info "✓ liquibase image"

# ----- Step 7: Run migrations (per project) -----
# Each project has its own changelog under liquibase/<name>/changelog/.
# Verify the directory exists for every project, then run Liquibase once
# per project against that project's DB.
log_step "Running Liquibase migrations..."
for name in $PROJECT_NAMES; do
    CHANGELOG_DIR="$SRC_DIR/liquibase/$name"
    if [[ ! -d "$CHANGELOG_DIR/changelog" ]]; then
        log_error "No liquibase changelog dir for project '$name' at $CHANGELOG_DIR/changelog"
    fi
    IFS=$'\t' read -r DB_HOST DB_PORT DB_NAME DB_USER DB_PASSWORD <<< "$(project_db_info "$name")"
    log_info "  [$name] migrating $DB_NAME..."
    docker run --rm \
        --network host \
        -v "$CHANGELOG_DIR:/liquibase/project:ro" \
        hobby-server-liquibase:latest \
        --searchPath=/liquibase/project \
        --changeLogFile=changelog/db.changelog-master.xml \
        --url="jdbc:postgresql://$DB_HOST:$DB_PORT/$DB_NAME" \
        --username="$DB_USER" \
        --password="$DB_PASSWORD" \
        update || log_warn "  [$name] migrations exit nonzero (already up-to-date is fine)"
done

# ----- Step 8: Restart server container -----
log_step "Starting hobby-server..."
if docker ps -a --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
    docker stop "$CONTAINER_NAME" >/dev/null 2>&1 || true
    docker rm "$CONTAINER_NAME" >/dev/null 2>&1 || true
fi

docker run -d \
    --name "$CONTAINER_NAME" \
    --restart unless-stopped \
    --network host \
    -v "$CONFIG_FILE:/config/config.yaml:ro" \
    hobby-server:latest \
    hobby-server --config /config/config.yaml || log_error "Failed to start container"

sleep 2
if docker ps --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
    log_info "✓ hobby-server running on :$SERVER_PORT (projects: $(echo $PROJECT_NAMES | tr '\n' ' '))"
else
    log_error "Container failed to start. Check: docker logs $CONTAINER_NAME"
fi

echo ""
log_info "Installation complete."
echo ""
echo "Next steps:"
for name in $PROJECT_NAMES; do
    echo "  - Add a user to project '$name':"
    echo "      docker run --rm --network host \\"
    echo "        -v \"$CONFIG_FILE:/config/config.yaml:ro\" \\"
    echo "        hobby-server:latest \\"
    echo "        add-user --config /config/config.yaml --project $name <username> <password>"
done
echo "  - Verify Apache proxies your public URLs to 127.0.0.1:$SERVER_PORT"
echo "  - Test:  curl -sS http://127.0.0.1:$SERVER_PORT/healthz"
echo ""
