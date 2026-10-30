# benchouse

`benchouse` is an open-source command-line tool, built for one time reviews of
your conversations. It reads your analytics agent's conversation history, takes
your dbt project and the agent's context, and uploads it with your permission
to our review system.

**What stays on your machine.** None of your data leaves your machine until
you approve the upload. Passwords, tokens and keys are used to read locally
and are never uploaded. **Users are pseudonymised:** user ids, names,
emails and IP addresses become pseudonyms (`person_<hash>`). Questions and
answers are not touched. By default it also leaves out the rows your agent's
queries returned and your user directory. The question, the SQL, the answer
and the row counts are kept.

## Install

**With one command** (macOS and Linux):

```sh
curl -fsSL https://raw.githubusercontent.com/benchouse/benchouse-cli/main/install.sh | sh
```

**Or download** it for your platform from the
[latest release](https://github.com/benchouse/benchouse-cli/releases/latest):

| macOS | Linux | Windows |
|---|---|---|
| [Apple silicon](https://github.com/benchouse/benchouse-cli/releases/latest/download/benchouse_darwin_arm64.tar.gz) | [x64](https://github.com/benchouse/benchouse-cli/releases/latest/download/benchouse_linux_amd64.tar.gz) | [x64](https://github.com/benchouse/benchouse-cli/releases/latest/download/benchouse_windows_amd64.exe) |
| [Intel](https://github.com/benchouse/benchouse-cli/releases/latest/download/benchouse_darwin_amd64.tar.gz) | [ARM](https://github.com/benchouse/benchouse-cli/releases/latest/download/benchouse_linux_arm64.tar.gz) | [ARM](https://github.com/benchouse/benchouse-cli/releases/latest/download/benchouse_windows_arm64.exe) |

## Run it

**macOS and Linux.** Run `benchouse` in a terminal. After a download, unpack it first:

```sh
tar -xzf benchouse_*.tar.gz && ./benchouse
```

**Windows.** Run the downloaded `.exe` in PowerShell or Windows Terminal.

It asks which agent you use and walks you through the rest, and nothing is
uploaded until you say yes.

With a download (not the one-command install), macOS may say the developer
can't be verified until our builds are notarised: run
`xattr -d com.apple.quarantine ./benchouse` once. Windows SmartScreen may ask
you to confirm.

---

[Every agent and flag](docs/usage.md) ·
[Running it from a coding agent](AGENTS.md) ·
[What's in an upload](docs/bundle-format.md) ·
[Usage stats](docs/usage.md#usage-stats) ·
[Privacy policy](https://benchouse.ai/privacy#cli-uploads) ·
MIT licensed
