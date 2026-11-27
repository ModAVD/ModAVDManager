package ui

// LaunchChromeWindow requests a separate Chrome app window on Apple Silicon
// macOS, falling back to the default browser if Chrome cannot be started.
func LaunchChromeWindow(url string) (*Window, error) { return launchChromeWindow(url, true) }
