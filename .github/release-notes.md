## Install

| System | Download |
| ------ | -------- |
| Windows 10/11 | `Nexora-<version>-windows-x64-setup.exe` |
| macOS 12+ (Apple Silicon and Intel) | `Nexora-<version>-macos-universal.dmg` |
| Linux | `Nexora-<version>-linux-x86_64.AppImage`, or the `.deb` for Debian/Ubuntu |

### Opening Nexora the first time

These builds are not yet signed with a paid certificate, so your system
warns before the first launch. The app is open source and built by this
repository's [release workflow](https://github.com/Diwanshumidha/nexora/actions/workflows/release.yml); you can
verify any download with `gh attestation verify <file> --repo Diwanshumidha/nexora`.

- **Windows:** "Windows protected your PC" → click **More info** → **Run anyway**.
- **macOS:** drag Nexora to Applications and open it. When macOS says it
  can't be opened, go to **System Settings → Privacy & Security**, scroll
  down and click **Open Anyway** (then **Open**). Or, in Terminal:
  `xattr -dr com.apple.quarantine /Applications/nexora.app`
- **Linux (AppImage):** `chmod +x Nexora-*.AppImage` and run it.

Nexora downloads the Node.js runtime its extensions need during the
first-run setup, so nothing else has to be installed.
