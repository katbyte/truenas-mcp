package chttp

// localNetworkHint is added to a dial that macOS refused with "no route to host", whatever the address, which is why it says "may": for a server on
// the LAN it is usually Local Network privacy denying a process that is not the app it was granted to, which happens when the terminal app is
// upgraded while an old copy of it keeps running. curl is exempt, so the server looks up from the shell and down from the tool.
const localNetworkHint = " (macOS Local Network privacy may be blocking this process: if the terminal app was updated while it was running, restart it; otherwise allow it under System Settings > Privacy & Security > Local Network)"
