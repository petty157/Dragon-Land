# 🐉 Dragon Land Asset Inspector & ADB Pull Utility

[![Python Version](https://img.shields.io/badge/python-3.7%2B-blue.svg)](https://www.python.org/)
[![ADB Required](https://img.shields.io/badge/ADB-Android%20Platform--Tools-green.svg)](https://developer.android.com/tools/releases/platform-tools)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Platform](https://img.shields.io/badge/platform-Windows%20%7C%20macOS%20%7C%20Linux-lightgrey.svg)]()

A lightweight, zero-dependency Python utility designed to inspect, count, and extract cached Unity asset bundles and full game data directories for **Dragon Land** (`es.socialpoint.DragonLand`) from connected Android devices and emulators via ADB.

---

## ✨ Features

- **Multi-Device Handling:** Detects all connected Android devices/emulators and prompts for selection if more than one is found.
- **Case-Insensitive Path Discovery:** Resolves target folders across storage paths (`/sdcard/Android/data` and `/storage/emulated/0/Android/data`) regardless of case variations.
- **Unity Asset Bundle Counting:** Scans `/files/UnityCache/Shared/` and counts cached hash folders while ignoring metadata files (`__info`, `__data`).
- **Live Transfer Streaming:** Streams standard `adb pull` progress output in real-time to your terminal.
- **Double-Click Friendly:** Native pause wrappers prevent terminal windows from closing automatically upon execution on Windows/macOS.
- **Zero Third-Party Dependencies:** Uses standard Python library modules exclusively (`os`, `sys`, `subprocess`).

---

## 📋 Prerequisites & Dependencies

### 1. Python 3.7+
Make sure Python is installed and added to your system's PATH.
* [Download Python](https://www.python.org/downloads/)

### 2. Android SDK Platform-Tools (`adb`)
`adb` must be installed and accessible globally via your terminal:

* **Windows:**
  1. Download [Android SDK Platform-Tools](https://developer.android.com/tools/releases/platform-tools).
  2. Extract the folder (e.g., `C:\platform-tools`).
  3. Add `C:\platform-tools` to your **System Environment Variables -> Path**.
* **macOS (via Homebrew):**
  ```bash
  brew install android-platform-tools
