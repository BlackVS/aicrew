// Package installer holds no code: its tests check install-aicrewd.sh, the
// hub installer at the repository's root. They check that it pins the
// release the CHANGELOG names, that it refuses an unverified binary, and
// they run its upgrade transaction in bash against real aicrewd builds.
package installer
