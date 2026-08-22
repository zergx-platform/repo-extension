# repo-tools-go

Go extension server (NATS) replacing the Rust `repo-tools` service. Forwards
each file/git/meta tool to the repo-manager HTTP service.

## Tools (14)

`read`, `write`, `edit`, `delete`, `grep`, `glob`, `ls`, `explore`,
`git-diff`, `git-blame`, `git-log`, `git-show`, `git-branches`, `git-restore`.

## Dependency

`forgejo.develop.10.199.64.20.nip.io/rucoder/extension-sdk-go` (git URL).

## Config

| Env | Default |
| --- | ------- |
| `RUCODER_REPO_MANAGER_URL` | `http://rucoder-repo-manager.develop.svc.cluster.local:80` |
| `NATS_URL` | `nats://nats.develop.svc.cluster.local:4222` |
