// SPDX-License-Identifier: AGPL-3.0-or-later

// Package appstoremetadata carries the canonical app-store presentation data.
//
// It is the single source both the public site and the management console read
// from, replacing the two hand-maintained copies that used to drift apart.
// Editing an app means editing exactly one file under data/apps/.
package appstoremetadata

import "embed"

// Files is the data directory, embedded so the API is one binary with no
// runtime filesystem or network dependency of its own.
//
//go:embed data
var Files embed.FS
