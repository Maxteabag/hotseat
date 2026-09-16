// Package version holds the release number. It is read by `hotseat --version`
// and by the release workflow, and must stay equal to hotseat/__init__.py's
// __version__ until the Python package is removed.
package version

// Version is the release number of this build.
const Version = "0.1.0"
