# Деплой Tolmach

Рекомендуемый вариант — Docker Compose. Бот работает в одном экземпляре:
long polling и SQLite-очередь не рассчитаны на горизонтальное масштабирование.

## 1. Конфигурация

Создайте несекретные настройки:

```sh
cp deploy/compose.env.example deploy/compose.env
```

Укажите в `deploy/compose.env` разрешённые Telegram ID:

```dotenv
TELEGRAM_ALLOWED_USER_IDS=123456789,987654321
```

Создайте файлы секретов:

```sh
mkdir -m 700 secrets
printf '%s\n' 'groq-key' > secrets/groq_api_key
printf '%s\n' 'soniox-key' > secrets/soniox_api_key
printf '%s\n' 'speechmatics-key' > secrets/speechmatics_api_key
printf '%s\n' 'telegram-token' > secrets/telegram_bot_token
chmod 644 secrets/*
chmod 600 deploy/compose.env
```

Compose монтирует secrets как read-only файлы. Для non-root процесса файлам
нужен режим `0644`; от других пользователей хоста их защищает каталог `0700`.
Каталог и конфигурация исключены из Git и Docker build context.

## 2. Запуск

```sh
docker compose build
docker compose run --rm bot --check
docker compose up -d
docker compose ps
```

Контейнер работает от UID `10001`, с read-only root filesystem и ограниченным
`tmpfs` для временных медиа. Автозапуск после сбоя и перезагрузки обеспечивает
`restart: unless-stopped`.

## Данные и восстановление заданий

SQLite-база и WAL хранятся в named volume `tolmach-data`. Каждая задача
попадает в БД до помещения в память.

- `queued` и прерванные `running` задачи восстанавливаются после запуска;
- при `SIGTERM` активная задача возвращается в очередь;
- готовый текст сначала сохраняется как `ready`, затем отправляется в Telegram;
- если публикация прервалась, сохранённый текст отправится после рестарта без
  повторного STT-запроса.

Доставка at-least-once: в редком случае аварии сразу после ответа Telegram
возможен дубль сообщения, но транскрипция не потеряется.

## Логи

Бот пишет однострочный JSON одновременно в два места:

1. `stderr` → Docker logging driver: 5 файлов по 10 МБ;
2. `/app/logs/tolmach.jsonl` → volume `tolmach-logs`: активный файл 20 МБ,
   10 архивов, 30 дней, gzip.

Persistent-логи переживают пересоздание контейнера. В них нет транскрипций,
медиа, ключей или Telegram `file_id`.

```sh
# Смотреть текущие логи
docker compose logs -f --tail=100 bot
docker compose logs --since=24h bot

# Только ошибки; требуется jq
docker compose logs --no-log-prefix bot \
  | jq -R 'fromjson? | select(.level == "ERROR")'

# События конкретной задачи
docker compose logs --no-log-prefix bot \
  | jq -R 'fromjson? | select(.job_id == 123)'

# Файлы постоянного архива
docker run --rm -v tolmach-logs:/logs:ro alpine:3.23 ls -lah /logs
```

В логах доступны `job_id`, user ID, провайдер, тип медиа, состояние очереди,
длительность, время обработки, языки, кеш и ошибки.

## Бэкап

SQLite работает в WAL-режиме, поэтому нельзя копировать только `tolmach.db` у
работающего контейнера. Остановите бот и архивируйте весь volume:

```sh
mkdir -p backups
docker compose stop bot
docker run --rm \
  -v tolmach-data:/data:ro \
  -v "$PWD/backups:/backup" \
  alpine:3.23 \
  tar -czf /backup/tolmach-data.tar.gz -C /data .
docker compose start bot
```

## Обновление

```sh
docker tag tolmach-bot:local tolmach-bot:rollback
docker compose build --pull bot
docker compose up -d --no-deps bot
docker compose logs -f --tail=100 bot
```

Для отката укажите в `compose.yaml` образ `tolmach-bot:rollback` и повторите
`docker compose up -d --no-deps bot`.

## Прокси

`127.0.0.1:7890` внутри контейнера указывает на сам контейнер. Если сервер
выходит в интернет напрямую, не задавайте `HTTP_PROXY` и `HTTPS_PROXY`.

Если прокси работает на Docker-хосте, используйте:

```dotenv
HTTPS_PROXY=http://host.docker.internal:7890
HTTP_PROXY=http://host.docker.internal:7890
```

И раскомментируйте `extra_hosts` в `compose.yaml`.

Для запуска без Docker доступен пример systemd unit:
[deploy/tolmach-bot.service](deploy/tolmach-bot.service).
