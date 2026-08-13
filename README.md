# Tolmach

Telegram-бот для быстрой транскрипции голосовых сообщений, видеокружков,
аудио и видео. Сервер работает как thin client: распознавание выполняют Groq,
Soniox или Speechmatics.

## Команды

```text
/settings                       текущие настройки
/language ru                    фиксированный язык
/language auto                  автоопределение языка
/provider groq                  основной провайдер
/diarization_provider soniox    провайдер диаризации
/translate en                   перевод ответа на транскрипцию
/cancel                         отмена обработки
```

Без аргумента `/translate` переводит на русский или на последний выбранный
язык. Команду нужно отправлять ответом на первое сообщение транскрипции.

## Локальный запуск

Требуется Go 1.26+. Заполните все нужные секреты в `.env`, включая Telegram-токен и `TELEGRAM_ALLOWED_USER_IDS`.

```sh
CGO_ENABLED=0 go build -trimpath -o tolmach-bot ./cmd/bot
./tolmach-bot --check
./tolmach-bot
```

Бот использует long polling, поэтому публичный HTTP-сервер не нужен.

## CLI для проверки провайдеров

Репозиторий также содержит утилиту для сравнения моделей на локальных файлах:

```sh
CGO_ENABLED=0 go build -trimpath -o transcribe ./cmd/transcribe

./transcribe recording.ogg
./transcribe --provider soniox recording.ogg
./transcribe --provider speechmatics recording.ogg
./transcribe --language auto recording.mp4
```

Результаты пишутся с правами `0600` в `transcripts/`. Полный список параметров:

```sh
./transcribe --help
```

## Разработка

```sh
CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 go vet ./...
```

Инструкция по Docker-деплою, secrets, логам, восстановлению и бэкапам:
[DEPLOYMENT.md](DEPLOYMENT.md).
