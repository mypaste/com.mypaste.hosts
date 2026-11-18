# Hosts

MyPaste extension `com.mypaste.hosts` (version `1.2.0`).

在设置窗口里管理 /etc/hosts：按文件里的注释分组查看条目，启用或停用、新增与删除地址和主机名，写入前自动保留上一份，每次写入的版本都能在页面里翻看并恢复。读取、解析与拼回都在扩展自己的进程里完成；写入先用你的权限，系统不允许时由 macOS 弹出一次管理员授权，文件始终是本机这一份，不经过网络。

From 1.2.0 the menu declares a system-wide shortcut, `⌥⌃H` (`Ctrl+Alt+H` in the manifest): the host registers it while the package is enabled and opens the settings window on the Hosts page, so the file is one chord away. The chord is skipped, never stolen, when it is already taken.

The list page keeps its header, search row, rail and footer in place and scrolls only the list card; the write preview and a stored version open on the whole frame so the file reads line by line.

This repository is the extension's own source home, split out of the former `mypaste-code-extensions` monorepo. It contains only this extension's tracked source files. The published `.mypaste-plugin` archives and the marketplace index live in [`mypaste-extensions-catalog`](https://github.com/mypaste/mypaste-extensions-catalog).

The manifest speaks MyPaste plugin protocol v3. Installation, capability mediation, and content-digest verification are performed by the MyPaste desktop host.

## Files

- `mypaste.plugin.json`: package identity and capability declaration.
- `panel.html`: the settings menu page, drawing the file's rows and calling `hosts.request`.
- `src/`: the extension's own Go process — reading, parsing and rebuilding the file, the private backups it keeps through `storage.plugin`, and the one administrator prompt when the file is root-owned.

## Build

```bash
cd src && CGO_ENABLED=0 go build -trimpath ./cmd/mypaste-hosts
```

The released archive carries one binary per platform under `bin/<goos>/<goarch>/` (darwin arm64 and amd64) plus the manifest and the page.

## License

MIT. See [LICENSE](LICENSE).
