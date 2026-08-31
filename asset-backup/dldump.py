import os
import subprocess
import sys

TARGET_PACKAGE = "es.socialpoint.DragonLand"
SEARCH_ROOTS = ["/sdcard/Android/data", "/storage/emulated/0/Android/data"]

SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))


def run_cmd(cmd):
    try:
        res = subprocess.run(
            cmd,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            check=True,
        )
        return res.stdout.strip()
    except (subprocess.CalledProcessError, FileNotFoundError):
        return None


def exit_pause(code=0):
    input("\nPress Enter to exit...")
    sys.exit(code)


def check_adb_installed():
    if run_cmd(["adb", "version"]) is None:
        print("[!] Error: 'adb' command not found.")
        print("    Ensure Android SDK Platform-Tools is installed and added to your system PATH.")
        exit_pause(1)


def get_connected_devices():
    output = run_cmd(["adb", "devices"])
    if not output:
        return []

    devices = []
    lines = output.splitlines()[1:]
    for line in lines:
        parts = line.strip().split()
        if len(parts) >= 2 and parts[1] == "device":
            devices.append(parts[0])
    return devices


def select_device():
    devices = get_connected_devices()
    if not devices:
        print("[!] No authorized Android devices found.")
        print("    Please connect your device/emulator and enable USB debugging.")
        exit_pause(1)

    if len(devices) == 1:
        print(f"[*] Detected device: {devices[0]}")
        return devices[0]

    print(f"\n[*] Found {len(devices)} connected devices:")
    for idx, serial in enumerate(devices, 1):
        print(f"  [{idx}] {serial}")

    while True:
        choice = input(f"\nSelect a device [1-{len(devices)}]: ").strip()
        if choice.isdigit() and 1 <= int(choice) <= len(devices):
            return devices[int(choice) - 1]
        print("[!] Invalid selection. Please enter a valid number.")


def find_game_directory_case_insensitive(serial):
    for base in SEARCH_ROOTS:
        cmd = [
            "adb",
            "-s",
            serial,
            "shell",
            f"find '{base}' -maxdepth 1 -iname '{TARGET_PACKAGE}' 2>/dev/null",
        ]
        out = run_cmd(cmd)
        if out:
            matched_path = out.splitlines()[0].strip()
            if matched_path:
                return matched_path

    return None


def count_cached_assets(serial, game_dir):
    find_cache_cmd = [
        "adb",
        "-s",
        serial,
        "shell",
        f"find '{game_dir}' -maxdepth 3 -iname 'Shared' 2>/dev/null",
    ]
    shared_path = run_cmd(find_cache_cmd)

    if not shared_path:
        find_cache_cmd = [
            "adb",
            "-s",
            serial,
            "shell",
            f"find '{game_dir}' -maxdepth 2 -iname 'UnityCache' 2>/dev/null",
        ]
        shared_path = run_cmd(find_cache_cmd)

    if not shared_path:
        return 0, None

    target_cache_dir = shared_path.splitlines()[0].strip()

    count_cmd = [
        "adb",
        "-s",
        serial,
        "shell",
        f"find '{target_cache_dir}' -mindepth 1 -maxdepth 1 -type d 2>/dev/null | wc -l",
    ]
    raw_count = run_cmd(count_cmd)

    if raw_count and raw_count.isdigit():
        return int(raw_count), target_cache_dir

    return 0, target_cache_dir


def pull_folder(serial, remote_path, local_destination):
    abs_dest = os.path.abspath(os.path.expanduser(local_destination))
    os.makedirs(abs_dest, exist_ok=True)

    print(f"\n[*] Pulling '{remote_path}' -> '{abs_dest}'...")
    cmd = ["adb", "-s", serial, "pull", remote_path, abs_dest]

    proc = subprocess.Popen(
        cmd,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        bufsize=1,
    )

    for line in proc.stdout:
        print(line, end="")

    proc.wait()

    if proc.returncode == 0:
        print(f"\n[+] Transfer complete! Files saved to:\n    {abs_dest}")
    else:
        print(f"\n[!] ADB pull finished with exit code {proc.returncode}.")


def main():
    print("=" * 60)
    print(" Dragon Land Asset Inspector & ADB Pull Utility")
    print("=" * 60)

    check_adb_installed()

    serial = select_device()

    print(f"\n[*] Searching for {TARGET_PACKAGE} folder (case-insensitive)...")
    game_dir = find_game_directory_case_insensitive(serial)

    if not game_dir:
        print(f"\n[!] Error: Could not find the folder for {TARGET_PACKAGE}.")
        print("    Ensure the game is installed and data has been generated.")
        exit_pause(1)

    print(f"[+] Found game directory: {game_dir}")

    asset_count, cache_dir = count_cached_assets(serial, game_dir)
    print("\n" + "-" * 60)
    print(" Asset Cache Status:")
    print(f"  Target Path : {cache_dir if cache_dir else 'UnityCache not found'}")
    print(f"  Total Assets: {asset_count} cached asset bundle folder(s)")
    print("-" * 60)

    default_dest = os.path.join(SCRIPT_DIR, "DragonLand_Full_Backup")
    print(f"\nDefault Destination: {default_dest}")
    user_dest = input("Enter destination folder on PC (or press Enter for default): ").strip()

    dest_path = user_dest if user_dest else default_dest

    pull_folder(serial, game_dir, dest_path)

    exit_pause(0)


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        print("\n\n[!] Operation cancelled by user.")
        exit_pause(1)