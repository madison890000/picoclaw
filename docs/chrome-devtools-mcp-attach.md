# Chrome DevTools MCP Attach Mode

This document explains the PicoClaw configuration needed to use `chrome-devtools-mcp` with an already-open Chrome browser.

## Background

`chrome-devtools-mcp` is not a fully generic MCP server in practice.

Although it speaks MCP over stdio, it also depends on a real Chrome debugging session being available on the host. That means treating it like a normal command-only MCP server can be fragile, especially when using `--browserUrl` with a manually managed debugging port.

PicoClaw now supports an attach-only configuration for this integration. In this mode, PicoClaw starts `chrome-devtools-mcp` with `--autoConnect` and attaches to an already-open Chrome instance instead of launching a dedicated browser.

## Supported Mode

Only attach mode is supported.

Launch mode is intentionally not supported in this integration path.

## Configuration

Use the following MCP server configuration:

```json
{
  "tools": {
    "mcp": {
      "enabled": true,
      "servers": {
        "chrome": {
          "enabled": true,
          "kind": "chrome-devtools",
          "mode": "attach",
          "channel": "stable"
        }
      }
    }
  }
}
```

## Fields

`kind`

Must be `chrome-devtools`. This tells PicoClaw that the server is a special integration and should not be treated as a fully generic MCP process definition.

`mode`

Must be `attach`. PicoClaw will reject unsupported values such as `launch`.

`channel`

Selects which installed Chrome channel to attach to. Supported values:

- `stable`
- `beta`
- `dev`
- `canary`

If omitted, PicoClaw defaults to `stable`.

## What PicoClaw Does Internally

When PicoClaw sees:

```json
{
  "kind": "chrome-devtools",
  "mode": "attach",
  "channel": "stable"
}
```

it normalizes that server to the equivalent stdio command:

```bash
npx -y chrome-devtools-mcp@latest --autoConnect
```

If `channel` is not `stable`, PicoClaw adds the corresponding `--channel=...` argument.

For example:

```json
{
  "kind": "chrome-devtools",
  "mode": "attach",
  "channel": "beta"
}
```

becomes:

```bash
npx -y chrome-devtools-mcp@latest --autoConnect --channel=beta
```

## Chrome-Side Setup

Before using the tools, Chrome itself must allow remote debugging attachment.

1. Open Chrome.
2. Visit `chrome://inspect/#remote-debugging`.
3. Enable remote debugging access as prompted by Chrome.
4. Keep that Chrome instance open.
5. Start or restart PicoClaw.

When `chrome-devtools-mcp` attaches, Chrome may show a permission prompt. That prompt must be accepted.

## Why Not Use `browserUrl`

A configuration like this may look valid:

```json
{
  "enabled": true,
  "command": "npx",
  "args": [
    "-y",
    "chrome-devtools-mcp@latest",
    "--browserUrl",
    "http://127.0.0.1:9222"
  ]
}
```

but it depends on a manually managed remote debugging endpoint being available and behaving exactly like a Chrome DevTools endpoint. In practice this often fails because:

- Chrome was not started with the expected debugging port.
- Another process is listening on the port.
- The endpoint returns `404` instead of DevTools metadata such as `/json/version`.
- The configuration is tightly coupled to host state that PicoClaw does not manage.

Attach mode avoids that class of failure and better matches the intended “use the already-open browser” workflow.

## Validation Rules

PicoClaw validates this integration during MCP server connection setup.

These cases are rejected:

- `kind: "chrome-devtools"` with `mode: "launch"`
- invalid `channel` values
- `url` transport for this integration in attach mode
- non-stdio transport for this integration in attach mode

## Troubleshooting

If `mcp_chrome_*` tools still cannot connect:

1. Confirm Chrome is already open.
2. Confirm the correct Chrome channel is selected in `channel`.
3. Confirm remote debugging is enabled in `chrome://inspect/#remote-debugging`.
4. Restart PicoClaw after changing configuration.
5. Check PicoClaw logs for MCP connection errors.

If the issue is channel mismatch, try changing:

```json
"channel": "stable"
```

to:

```json
"channel": "beta"
```

or another installed Chrome channel.

## Recommended Usage

For `chrome-devtools-mcp`, prefer the special attach-only configuration instead of raw `command` plus `args` configuration.

That keeps the user-facing config aligned with the real execution model: attach to an existing browser session, not launch and manage a separate browser instance.
