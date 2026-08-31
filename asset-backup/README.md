```markdown
# 🐉 Dragon Land Asset Inspector & Dumper (`dl_dumper_gui`)

[![Python Version](https://img.shields.io/badge/python-3.7%2B-blue.svg)](https://www.python.org/)
[![Android ADB](https://img.shields.io/badge/Android-ADB%20Supported-green.svg)](https://developer.android.com/tools/releases/platform-tools)
[![iOS Jailbreak](https://img.shields.io/badge/iOS-Jailbreak%20%2F%20OpenSSH-red.svg)](https://ios.cfw.guide/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

A unified GUI utility designed to inspect, count, and extract cached Unity asset bundles and full game data containers for **Dragon Land** (`es.socialpoint.DragonLand`) from both **Android** devices (via ADB) and **Jailbroken iOS** devices (via SSH over USB).

---

## 📋 Features

- 🖥️ **Cross-Platform GUI:** Built with Python `tkinter` — no terminal commands required during extraction.
- 🤖 **Android Support:** Automatic ADB device discovery, case-insensitive package checks, and live folder streaming.
- 🍏 **iOS Support:** Bypasses Apple's sandbox via OpenSSH & SFTP over a high-speed USB tunnel.
- 📦 **UnityCache Counter:** Counts individual asset bundle hashes in `UnityCache/Shared/` without counting metadata files (`__info`, `__data`).
- 📁 **Folder Picker:** Built-in desktop file dialog to choose your backup directory.

---

## ⚙️ Prerequisites & Installation

### 1. Python & Python Dependencies
Ensure Python 3.7+ is installed. Install `paramiko` for iOS SFTP transfers:

```bash
pip install paramiko

```

---

## 🤖 Android Setup Guide

### 1. Requirements

* **Android SDK Platform-Tools (`adb`)**: Must be in your system PATH.
* **Windows:** Download [Platform-Tools](https://developer.android.com/tools/releases/platform-tools) and add the folder to your Environment Variables.
* **macOS:** `brew install android-platform-tools`
* **Linux:** `sudo apt install adb`



### 2. Device Setup

1. Go to **Settings > About Phone** and tap **Build Number** 7 times to unlock Developer Options.
2. Go to **Settings > Developer Options** and toggle on **USB Debugging**.
3. Plug the phone into your PC via USB and authorize the computer prompt.

---

## 🍏 iOS Setup Guide

### 1. Why Jailbreaking is Required

Apple strictly sandboxes iOS applications:

* App cache folders (`/var/mobile/Containers/Data/Application/<UUID>/Library/Caches/`) are locked.
* Standard iTunes/iCloud backups **intentionally skip `Library/Caches**` (where Unity saves downloaded assets).
* Jailbreaking removes this restriction, letting OpenSSH read and extract the cache directly.

### 2. How to Jailbreak Your Device

Jailbreaking depends on your exact device model and iOS version:

👉 **Follow the official guide: [iOS CFW Guide (https://ios.cfw.guide/)**](https://ios.cfw.guide/)

1. Visit [ios.cfw.guide/get-started](https://ios.cfw.guide/get-started/).
2. Select your device and iOS version (found in **Settings > General > About**).
3. Follow the matching instructions:
* **Dopamine:** For iOS 15.0 – 16.6.1 / 17.0 (A12+ devices).
* **palera1n:** For iPhone X and older on iOS 15.0 – 18.x.



### 3. Set Up OpenSSH & USB Forwarding

1. **On your iOS Device:** Open **Sileo** (or Zebra), search for `OpenSSH`, and install it.
2. **On your PC:** Install `libimobiledevice` to get `iproxy`:
* **Windows:** Download precompiled binaries or install via MSYS2 / `winget install libimobiledevice`.
* **macOS:** `brew install libimobiledevice`
* **Linux:** `sudo apt install libimobiledevice-utils`


3. **Start the USB Tunnel:** Plug in your iPhone and run this command in a terminal:
```bash
iproxy 2222 22

```


*(Keep this terminal open while using the tool).*

---

## 🚀 How to Run the Tool

1. Clone or download this repository:
```bash
git clone [https://github.com/your-username/dragonland-dumper.git](https://github.com/your-username/dragonland-dumper.git)
cd dragonland-dumper

```


2. Run the GUI script:
```bash
python dl_dumper_gui.py

```


*(You can also double-click `dl_dumper_gui.py` on Windows/macOS).*
3. **Using the GUI:**
* **Step 1:** Choose **Android (ADB)** or **iOS Jailbreak (SSH/USB)**.
* **Step 2:** Click **Inspect & Count Assets** to verify the game files and see your total bundle count.
* **Step 3:** Click **Browse...** to select your save destination.
* **Step 4:** Click **Pull Entire Game Directory** to download everything to your PC.



---

## 📂 Target Asset Structure

The tool inspects and extracts the following internal directories:

### Android:

```text
Android/data/es.socialpoint.DragonLand/
└── files/
    └── UnityCache/
        └── Shared/
            ├── <bundle_hash_1>/   <-- Counted
            ├── <bundle_hash_2>/   <-- Counted
            └── ...

```

### iOS:

```text
/var/mobile/Containers/Data/Application/<APP-UUID>/
└── Library/
    └── Caches/
        └── UnityCache/
            └── Shared/
                ├── <bundle_hash_1>/   <-- Counted
                ├── <bundle_hash_2>/   <-- Counted
                └── ...

```

---

## 🛠️ Troubleshooting

| Issue | Platform | Cause / Fix |
| --- | --- | --- |
| `adb: command not found` | Android | ADB is not in system PATH. Verify `adb version` works in CMD/Terminal. |
| `No authorized Android devices detected` | Android | Unlock phone and tap "Always allow from this computer" on the USB prompt. |
| `iOS SSH Connection Failed` | iOS | Ensure `iproxy 2222 22` is actively running in a separate terminal. |
| `OpenSSH authentication error` | iOS | Verify default password (`alpine`) or update it in the GUI if changed. |
| `UnityCache not found` | Both | Launch the game at least once so the initial cache directories are generated. |

---

## 📄 License

This project is licensed under the [MIT License](https://www.google.com/search?q=LICENSE).

*Disclaimer: This tool is intended for personal data preservation and game archiving purposes. Dragon Land is a registered trademark of Social Point.*

```

```
