#!/usr/bin/env bash
# Именованные действия выката вместо произвольного curl к Coolify.
#
# Нужен, потому что с 17.08.2026 выкат идёт руками: минуты GitHub Actions
# исчерпаны, и workflow Deploy не запускается. Порядок шагов здесь тот же, что
# в .github/workflows/deploy.yml, - файл остаётся каноном, скрипт лишь даёт
# выполнить его человеку.
#
# Репозиторий публичный, поэтому ни адрес Coolify, ни uuid сервиса в него не
# попадают: и то и другое приходит из окружения либо из Keychain. Токен наружу
# не печатается ни при какой ошибке.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
compose="$repo_root/deploy/prod/docker-compose.coolify.yml"
image_name="ghcr.io/daniil4545/tg-intake"

usage() {
    cat <<'USAGE'
Usage: deploy/coolify.sh <action> [args...]

  status              сервис в Coolify: имя, статус (read-only)
  sync-compose        залить deploy/prod/docker-compose.coolify.yml в сервис
  set-image <commit>  APP_IMAGE = <образ>:sha-<commit>
  deploy              запустить выкат (force=true) и проверить ответ
  release <commit>    sync-compose, set-image, deploy - в этом порядке

Порядок обязателен: сначала Compose, потом переменная, потом выкат.
Ответ 200 означает «Coolify принял запрос», а не «версия выкачена»: сверять
app_revision через deploy/safe-ssh.sh health.

Секреты берутся из окружения, иначе из Keychain:
  COOLIFY_TOKEN          <- служба galera-coolify-token
  COOLIFY_API_BASE       <- служба galera-coolify-api
  INTAKE_SERVICE_UUID    <- служба intake-service-uuid
USAGE
}

# from_keychain: значение секрета не должно попадать ни в аргументы команды, ни
# в лог - только в переменную вызывающего.
from_keychain() {
    security find-generic-password -s "$1" -w 2>/dev/null || true
}

need() {
    local name="$1" service="$2" value="${!1:-}"
    if [[ -z "$value" ]]; then
        value="$(from_keychain "$service")"
    fi
    if [[ -z "$value" ]]; then
        echo "$name не задан: ни в окружении, ни в Keychain (служба $service)" >&2
        exit 2
    fi
    printf '%s' "$value"
}

commit_arg() {
    local value="${1:-}"
    if [[ ! "$value" =~ ^[0-9a-f]{40}$ ]]; then
        echo "нужен полный sha коммита (40 hex-символов)" >&2
        exit 2
    fi
    printf '%s' "$value"
}

api() {
    local method="$1" path="$2"
    shift 2
    curl --fail --silent --show-error --request "$method" \
        "${api_base}${path}" \
        --header "Authorization: Bearer ${token}" \
        --header "Content-Type: application/json" \
        "$@"
}

sync_compose() {
    test -s "$compose" || { echo "нет файла $compose или он пуст" >&2; exit 1; }
    # docker compose config тут не годится: файл писан под Coolify и содержит
    # его расширения (exclude_from_hc), на которых валидатор Compose падает, а
    # значения переменных приходят из контура, не из репозитория. Проверяем то,
    # что можно проверить локально: файл на месте и объявляет ожидаемый стек.
    for service in postgres migrate app; do
        grep -q "^  ${service}:" "$compose" || {
            echo "в $compose нет сервиса $service" >&2
            exit 1
        }
    done
    # openssl, а не base64: у GNU и BSD разные флаги переноса строк, а Coolify
    # ждёт одну строку.
    local payload
    payload="$(jq -n --arg c "$(openssl base64 -A -in "$compose")" '{docker_compose_raw: $c}')"
    api PATCH "/services/${uuid}" --data "$payload" --output /dev/null
    echo "compose synced"
}

set_image() {
    local commit image
    commit="$(commit_arg "${1:-}")"
    image="${image_name}:sha-${commit}"
    api PATCH "/services/${uuid}/envs" \
        --data "$(jq -n --arg v "$image" '{key: "APP_IMAGE", value: $v}')" --output /dev/null
    echo "APP_IMAGE=${image}"
}

run_deploy() {
    # force=true обязателен для Compose-сервиса: без него Coolify отвечает
    # «Service started», видит сервис запущенным и контейнеры не пересоздаёт -
    # контур остаётся на прежнем образе при успешном ответе.
    local response
    response="$(api GET "/deploy?uuid=${uuid}&force=true")"
    echo "coolify response: ${response}"
    grep -q "deployments" <<<"$response" || { echo "Coolify не принял выкат" >&2; exit 1; }
    echo "deploy queued"
}

action="${1:-}"
case "$action" in
    status | sync-compose | set-image | deploy | release) ;;
    *) usage; exit 2 ;;
esac

token="$(need COOLIFY_TOKEN galera-coolify-token)"
api_base="$(need COOLIFY_API_BASE galera-coolify-api)"
uuid="$(need INTAKE_SERVICE_UUID intake-service-uuid)"

case "$action" in
    status)
        api GET "/services/${uuid}" | jq -r '"name=\(.name) status=\(.status // "unknown")"'
        ;;
    sync-compose)
        sync_compose
        ;;
    set-image)
        set_image "${2:-}"
        ;;
    deploy)
        run_deploy
        ;;
    release)
        commit="$(commit_arg "${2:-}")"
        sync_compose
        set_image "$commit"
        run_deploy
        ;;
esac
