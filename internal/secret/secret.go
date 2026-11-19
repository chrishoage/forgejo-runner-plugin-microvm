// SPDX-License-Identifier: GPL-3.0-or-later

// Package secret holds the secret value type, which implements
// slog.LogValuer and redacts itself, so a secret logged by accident shows no
// value (LOG-2).
package secret
