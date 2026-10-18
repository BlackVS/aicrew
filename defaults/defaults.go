// Package defaults embeds the default automations each release ships
// (docs/DESIGN-TURNS.md, 13.1): the scripts under automations/ and their
// entries in automations/subscriptions.json, in the format of a process
// manifest's automations entries with the source "release". A process
// manifest's entry of the same name replaces or disables one of them.
//
// The path is kept apart from a repository's own automations/, which holds
// aicrew's project automations like any other repository's (9.3).
package defaults

import "embed"

// Automations holds automations/subscriptions.json and the scripts it
// names, under automations/.
//
//go:embed automations
var Automations embed.FS

// AutomationsFile is the entries' file in Automations.
const AutomationsFile = "automations/subscriptions.json"
