[worker review] reviewed at head c1102a83950fa837895f41cfb7e03e37223b9d1b

CURRENT OBJECTIVE
A numeric or malformed `shutdown_timeout` in aicrewd's configuration is refused with an error that names `shutdown_timeout` and still says what a valid value looks like; nothing else is accepted or refused differently.

BLOCKERS
- None

CURRENT-SCOPE IMPROVEMENTS
- None

FOLLOW-UPS
- None

OBSERVATIONS
- [OBSERVATION] [CONFIRMED] `decodeConfig` labels every `errNotDuration` as `shutdown_timeout` (internal/svcconfig/config.go:187). This holds because `ShutdownTimeout` (config.go:34) is the one `Duration` field, and the comment at config.go:188 says so. A second `Duration` field would need the field name carried in the error instead; no such field exists or is in scope.
- [OBSERVATION] [CONFIRMED] `decodeConfig` is the one decode path for the file: `ParseConfig` (config.go:175) and the migration (migrate.go:184) both call it, so the message changes in both and nowhere else. `encoding/json` returns an `Unmarshaler`'s error unwrapped, so `errors.Is` matches; the two new and changed table cases in `internal/svcconfig` and `internal/server` prove it and fail if the mapping regresses.
- [OBSERVATION] [CONFIRMED] What is accepted or refused is unchanged: `UnmarshalJSON` refuses the same inputs as before (including `null`, which still fails `time.ParseDuration("")`); only the text differs. The negative and too long cases keep their `validate` messages, and the absent field still takes the default (config.go:197).

REJECTED
- None

RISK ACCEPTED
- None

VERDICT
READY_FOR_HUMAN_MERGE
No blocker: both inputs are refused with an error naming `shutdown_timeout` and keeping the hint, both test tables assert it, and the observations do not block.

RISK
LOW — error text of one configuration field; no change of accepted input, routes, authentication or store.

Review: oh-code-review (high) via Claude Code
