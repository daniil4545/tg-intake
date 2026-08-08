#!/usr/bin/env bash
# Именованные read-only команды к серверу вместо произвольного ssh.
# Все действия только читают: ни одно из них не меняет состояние контура.
set -euo pipefail

host="${INTAKE_SSH_HOST:-root@5.42.118.105}"
resource="${INTAKE_RESOURCE:-intake-dev}"
action="${1:-}"
arg="${2:-}"

usage() {
    cat <<'USAGE'
Usage: deploy/safe-ssh.sh <action> [args...]

Read-only actions:
  inventory        контейнеры и образы контура
  health           ревизия образа, health контейнеров, код выхода миграций
  app-logs [N]     логи приложения, по умолчанию 200 строк, максимум 1000
  db-logs [N]      логи PostgreSQL, по умолчанию 200 строк, максимум 1000
  app-env          переменные окружения приложения, значения скрыты
  db-counts        число строк по таблицам, без содержимого

INTAKE_SSH_HOST переопределяет root@5.42.118.105.
INTAKE_RESOURCE переопределяет контур intake-dev.
USAGE
}

tail_lines() {
    local value="${1:-200}"
    if [[ ! "$value" =~ ^[0-9]+$ ]]; then
        echo "tail-lines must be a number" >&2
        exit 2
    fi
    if (( value > 1000 )); then
        value=1000
    fi
    echo "$value"
}

ssh_opts=(
    -o BatchMode=yes
    -o StrictHostKeyChecking=accept-new
    -o ConnectTimeout=10
)

filter="--filter label=coolify.resourceName=$resource"
lines=""
remote=""

case "$action" in
    inventory)
        remote="docker ps -a $filter --format 'service={{.Label \"com.docker.compose.service\"}} name={{.Names}} image={{.Image}} status={{.Status}}' | sort"
        ;;
    health)
        # migrate - one-shot: отсутствие контейнера после очистки не ошибка,
        # ошибка - существующий контейнер с ненулевым кодом выхода.
        remote="app=\$(docker ps -q $filter --filter 'label=com.docker.compose.service=app' | head -1); db=\$(docker ps -q $filter --filter 'label=com.docker.compose.service=postgres' | head -1); mig=\$(docker ps -aq $filter --filter 'label=com.docker.compose.service=migrate' | head -1); test -n \"\$app\" || { echo 'app=not_running'; exit 1; }; test -n \"\$db\" || { echo 'postgres=not_running'; exit 1; }; docker inspect -f 'app_revision={{index .Config.Labels \"org.opencontainers.image.revision\"}} app_health={{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}} app_restart={{.HostConfig.RestartPolicy.Name}}' \"\$app\"; docker inspect -f 'postgres_health={{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' \"\$db\"; if test -n \"\$mig\"; then echo \"migrate_state=\$(docker inspect -f '{{.State.Status}}:{{.State.ExitCode}}' \"\$mig\")\"; else echo 'migrate_state=absent'; fi; test \"\$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' \"\$app\")\" = healthy"
        ;;
    app-logs)
        lines="$(tail_lines "$arg")"
        remote="c=\$(docker ps -q $filter --filter 'label=com.docker.compose.service=app' | head -1); test -n \"\$c\" || { echo 'app=not_running'; exit 1; }; docker logs --tail $lines \"\$c\" 2>&1"
        ;;
    db-logs)
        lines="$(tail_lines "$arg")"
        remote="c=\$(docker ps -q $filter --filter 'label=com.docker.compose.service=postgres' | head -1); test -n \"\$c\" || { echo 'postgres=not_running'; exit 1; }; docker logs --tail $lines \"\$c\" 2>&1"
        ;;
    db-counts)
        # Только счётчики: строки таблиц содержат ФИО автора и текст обращения,
        # им место в базе, а не в терминале.
        remote="c=\$(docker ps -q $filter --filter 'label=com.docker.compose.service=postgres' | head -1); test -n \"\$c\" || { echo 'postgres=not_running'; exit 1; }; docker exec \"\$c\" psql -U intake -d intake -tAc \"select 'projects='||(select count(*) from projects)||' users='||(select count(*) from users)||' cases='||(select count(*) from cases)||' case_items='||(select count(*) from case_items)||' jobs='||(select count(*) from jobs)\""
        ;;
    app-env)
        # Значения скрыты намеренно: в окружении лежат токен бота и пароль базы.
        remote="c=\$(docker ps -q $filter --filter 'label=com.docker.compose.service=app' | head -1); test -n \"\$c\" || { echo 'app=not_running'; exit 1; }; docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' \"\$c\" | sed 's/=.*/=<set>/' | sort"
        ;;
    *)
        usage
        exit 2
        ;;
esac

exec ssh "${ssh_opts[@]}" "$host" "$remote"
