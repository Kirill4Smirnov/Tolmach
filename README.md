# Tolmach

Thin-client tools for transcribing Telegram voice messages and video notes.
The first milestone is a local CLI for comparing Groq Whisper, Speechmatics, and Soniox
on real recordings before the Telegram bot is built.

## Requirements

- Go 1.26 or newer;
- a Groq, Speechmatics, and/or Soniox API key;
- audio or video accepted by the selected provider.

FFmpeg is not required for this evaluation CLI. Files are streamed in their
original format and are not loaded fully into RAM.

## Configuration

Either export the key in your shell:

```sh
export GROQ_API_KEY='gsk_...'
export SPEECHMATICS_API_KEY='...'
export SONIOX_API_KEY='...'
```

or copy `.env.example` to `.env` and put the key there. `.env` is ignored by
Git. Existing environment variables take precedence over values in the file.

If Groq must be reached through a local HTTP proxy, put it in `.env` as well:

```dotenv
HTTPS_PROXY=http://127.0.0.1:7890
HTTP_PROXY=http://127.0.0.1:7890
```

The CLI loads `.env` before constructing its HTTP client, so Go applies these
standard proxy variables automatically. `HTTPS_PROXY` controls HTTPS API
endpoints; `HTTP_PROXY` is included for consistency. Do not commit proxy
credentials if the URL contains a username or password.

## Usage

Build the program:

```sh
CGO_ENABLED=0 go build -trimpath -o transcribe ./cmd/transcribe
```

Put private test recordings under `recordings/` or `voices/`. Both directories
and the generated `transcripts/` directory are ignored by Git.

Validate a file without calling an API:

```sh
./transcribe --dry-run recordings/recording.ogg
```

Transcribe Russian speech with the more accurate default model:

```sh
./transcribe recordings/recording.ogg
```

By default, the CLI supplies Whisper with a neutral Russian transcript-style
prompt. On noisy test recordings this improved both recognition and punctuation
substantially without supplying recording-specific facts. Disable it for an
unconditioned comparison:

```sh
./transcribe --prompt '' recordings/recording.ogg
```

For unusual names and terminology, replace it with a short representative hint:

```sh
./transcribe --prompt 'Костя, Архыз, Карачаево-Черкесия, eSIM, Type-C.' recordings/recording.ogg
```

Use the cheaper, faster Turbo model:

```sh
./transcribe --model whisper-large-v3-turbo recordings/recording.ogg
```

Restore punctuation in a separate, guarded output:

```sh
./transcribe --polish recordings/recording.ogg
```

The punctuation pass is allowed to change only capitalization, punctuation,
and paragraphs. The CLI compares every word locally and rejects the polished
version if the text model adds, deletes, replaces, or reorders lexical content.
The raw Whisper transcript is always kept alongside `*.polished.txt`.

## Speechmatics and diarization

The recommended mode for Russian conversations is Enhanced, an explicit
Russian language pack, and acoustic speaker diarization:

```sh
./transcribe \
  --provider speechmatics \
  --language ru \
  recordings/dialog.ogg
```

This writes three private files: plain text, the raw `json-v2` response, and a
readable transcript containing timestamps and `S1`, `S2`, etc. By default the
remote Speechmatics job is deleted only after all local files have been written
successfully. Pass `--keep-remote-job` for debugging.

Automatic language identification detects one predominant language. It works
best with at least 60 seconds of speech, so it is an option rather than the
default for short Telegram messages:

```sh
./transcribe \
  --provider speechmatics \
  --language auto \
  --expected-languages ru,en \
  recordings/long-message.ogg
```

For genuine code-switching or several languages in one recording, use the
Batch-only multilingual Melia 1 model:

```sh
./transcribe \
  --provider speechmatics \
  --speechmatics-model melia-1 \
  --language multi \
  --language-hints ru,en \
  recordings/multilingual.mp4
```

Melia 1 supports diarization but currently lacks custom vocabulary, speaker
identification, confidence scores, and speech-intelligence add-ons. Enhanced
supports custom vocabulary (`--vocab`), punctuation sensitivity,
`--remove-disfluencies`, and both speaker and channel diarization. Channel
diarization is useful only when different speakers are actually recorded on
separate audio channels.

## Soniox and diarization

Soniox V5 is available as another cloud diarization backend. The CLI uses its
asynchronous API, which Soniox recommends when diarization quality matters:

```sh
./transcribe \
  --provider soniox \
  --language ru \
  recordings/dialog.ogg
```

This uploads the media once, polls the transcription, and writes plain text,
token JSON, and a timestamped speaker transcript. After successful local
writing—or after a partial API failure—the CLI deletes both the remote
transcription and uploaded file. `--keep-remote-job` keeps both for debugging.

Use automatic multilingual recognition and optional language hints like this:

```sh
./transcribe \
  --provider soniox \
  --language auto \
  --language-hints ru,en \
  recordings/dialog.ogg
```

`--soniox-language-hints-strict` restricts recognition to those languages.
For unusual names, product names, or a short description of the recording, use
`--vocab 'Tolmach,Soniox'` and `--soniox-context 'Informal technical meeting'`.
Soniox context is deliberately separate from the Whisper `--prompt`, since the
providers interpret those controls differently. Pass `--diarization none` if
speaker labels are not needed.

Useful tuning examples:

```sh
./transcribe \
  --provider speechmatics \
  --vocab 'Tolmach,Speechmatics,Архыз' \
  --speaker-sensitivity 0.6 \
  --prefer-current-speaker=true \
  --punctuation-sensitivity 0.5 \
  recordings/dialog.ogg
```

Use Groq automatic language detection:

```sh
./transcribe --language auto recordings/recording.mp4
```

Ask for word timestamps and provide vocabulary hints:

```sh
./transcribe \
  --timestamps segment,word \
  --prompt 'Tolmach, Groq, Telegram' \
  recordings/recording.ogg
```

The program writes private (`0600`) files under `transcripts/`:

```text
recording.whisper-large-v3-turbo.ru.transcript.txt
recording.whisper-large-v3-turbo.ru.transcript.json
```

It does not print transcript text to the terminal unless `--print` is passed.
Existing results are protected; use `--overwrite` to replace them.

The default local input limit is 25 MiB, matching the Groq free-tier limit and
comfortably covering normal Telegram bot downloads. Change it with `--max-mib`,
or pass `--max-mib 0` when evaluating larger Speechmatics inputs.

## Evaluation procedure

For useful comparison, run both models on the same files and keep the default
temperature at zero:

```sh
./transcribe --model whisper-large-v3-turbo recordings/sample.ogg
./transcribe recordings/sample.ogg
```

Compare proper names, numbers, punctuation, hallucinations during silence,
mixed Russian/English phrases, latency, and speaker assignment. Groq Whisper
does not provide speaker diarization; Speechmatics and Soniox do.

## Tests

```sh
CGO_ENABLED=0 go test ./...
```
