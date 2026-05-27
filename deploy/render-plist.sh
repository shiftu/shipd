#!/usr/bin/env bash
# Render the shipd LaunchAgent plist to stdout. Invoked by `make install`.
#
# Inputs (all passed by the Makefile, defaults shown for direct invocation):
#   PREFIX         binary directory (default /opt/homebrew/bin)
#   LAUNCH_LABEL   launchd job label (default lol.jiangtao.shipd)
#   DATA_DIR       runtime state dir (default $HOME/.config/shipd/data)
#   LOG_DIR        log dir (default $HOME/.config/shipd/logs)
#   LISTEN_ADDR    HTTP bind, e.g. :8080
#   PUBLIC_BASE    external base URL, e.g. https://shipd.jiangtao.lol
#
# The plist is overwritten in place on every install; do not hand-edit the
# generated file — edit this script or the Makefile defaults.
set -euo pipefail

: "${PREFIX:=/opt/homebrew/bin}"
: "${LAUNCH_LABEL:=lol.jiangtao.shipd}"
: "${DATA_DIR:=$HOME/.config/shipd/data}"
: "${LOG_DIR:=$HOME/.config/shipd/logs}"
: "${LISTEN_ADDR:=:8080}"
: "${PUBLIC_BASE:=}"

# Build the optional --public-base-url argument so plists without one omit the
# flag entirely (server falls back to deriving the host from the request).
public_base_args=""
if [ -n "$PUBLIC_BASE" ]; then
  public_base_args=$'\t\t<string>--public-base-url</string>\n\t\t<string>'"$PUBLIC_BASE"$'</string>'
fi

cat <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>${LAUNCH_LABEL}</string>
	<key>ProgramArguments</key>
	<array>
		<string>${PREFIX}/shipd</string>
		<string>serve</string>
		<string>--data-dir</string>
		<string>${DATA_DIR}</string>
		<string>--addr</string>
		<string>${LISTEN_ADDR}</string>
${public_base_args}
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key>
		<string>${HOME}</string>
		<key>PATH</key>
		<string>/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>StandardOutPath</key>
	<string>${LOG_DIR}/shipd.log</string>
	<key>StandardErrorPath</key>
	<string>${LOG_DIR}/shipd.error.log</string>
	<key>WorkingDirectory</key>
	<string>${DATA_DIR}</string>
</dict>
</plist>
PLIST
