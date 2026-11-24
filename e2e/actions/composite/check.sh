#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
#
# The composite action's step: it exits non-zero unless its argument is
# right and the JavaScript action's GITHUB_ENV variable reached it, then
# sets the action's output.
set -eu
echo "composite action: hello, $1"
test "$1" = microvm
test "${FROM_JS:-}" = yes
echo "greeting=hi, $1" >>"$GITHUB_OUTPUT"
