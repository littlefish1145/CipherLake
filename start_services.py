#!/usr/bin/env python3
"""CipherLake Crypto Services Launcher - starts all services with color-coded logs."""

import subprocess
import sys
import os
import signal
import threading
import time
from datetime import datetime

# Service definitions: (name, executable, args, color, env)
SERVICES = [
    ("token-service",     "token-service.exe",     ["-port", "50051", "-key-path", "./data/keys/token"],     "\033[96m",   None),
    ("keygen-service",    "keygen-service.exe",     ["-port", "50052", "-key-path", "./data/keys/keygen"],    "\033[93m",   None),
    ("keyunwrap-service", "keyunwrap-service.exe",  ["-port", "50053", "-key-path", "./data/keys/keygen"],    "\033[95m",   None),
    ("encrypt-service",   "encrypt-service.exe",    ["-port", "50054"],                                     "\033[92m",   None),
    ("decrypt-service",   "decrypt-service.exe",    ["-port", "50055"],                                     "\033[94m",   None),
    ("keystore-service",  "keystore-service.exe",   ["-port", "50056", "-data-path", "./data/keystore"],     "\033[97m",   None),
    ("sts-service",       "sts-service.exe",        ["-port", "50057"],                                     "\033[35m",   None),
    ("cipherlake",             "cipherlake.exe",              [],                                                     "\033[33m",   {"CIPHERLAKE_IAM_ENABLED": "true", "CIPHERLAKE_IAM_STS_SERVICE_ADDR": "localhost:50057"}),
]

RESET = "\033[0m"
BOLD = "\033[1m"
DIM = "\033[2m"
RED = "\033[91m"
GRAY = "\033[90m"

processes = []
lock = threading.Lock()
stopping = False


def timestamp():
    return datetime.now().strftime("%H:%M:%S.") + f"{datetime.now().microsecond // 1000:03d}"


def is_error_log(text):
    """Detect if a log line is an error/warning level (zap JSON format)."""
    import json
    try:
        obj = json.loads(text)
        level = obj.get("level", "")
        if level in ("error", "fatal", "panic", "warn", "warning"):
            return True
        # Also check plain text patterns
    except (json.JSONDecodeError, ValueError):
        pass
    # Fallback: check plain text
    lower = text.lower()
    for kw in ["error", "fatal", "panic", "failed", "failure"]:
        if kw in lower:
            return True
    return False


def read_stream(name, color, stream, is_stderr):
    """Read from a stream and print with color prefix."""
    for line in iter(stream.readline, b""):
        if stopping:
            break
        text = line.decode("utf-8", errors="replace").rstrip()
        if not text:
            continue
        ts = timestamp()
        with lock:
            err = is_stderr and is_error_log(text)
            if err:
                print(f"{GRAY}{ts}{RESET} {RED}{BOLD}[{name:>18s} ERR]{RESET} {RED}{text}{RESET}", flush=True)
            else:
                print(f"{GRAY}{ts}{RESET} {color}{BOLD}[{name:>18s}]{RESET} {color}{text}{RESET}", flush=True)


def start_service(name, exe, args, color, env):
    """Start a single service process."""
    cmd = [exe] + args
    try:
        proc_env = os.environ.copy()
        if env:
            proc_env.update(env)

        proc = subprocess.Popen(
            cmd,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=proc_env,
            creationflags=subprocess.CREATE_NEW_PROCESS_GROUP if sys.platform == "win32" else 0,
        )
        processes.append((name, proc))

        t_out = threading.Thread(target=read_stream, args=(name, color, proc.stdout, False), daemon=True)
        t_err = threading.Thread(target=read_stream, args=(name, color, proc.stderr, True), daemon=True)
        t_out.start()
        t_err.start()

        ts = timestamp()
        with lock:
            print(f"{GRAY}{ts}{RESET} {color}{BOLD}[{name:>18s}]{RESET} {color}started (pid={proc.pid}){RESET}", flush=True)
        return proc
    except FileNotFoundError:
        ts = timestamp()
        with lock:
            print(f"{GRAY}{ts}{RESET} {RED}{BOLD}[{name:>18s}]{RESET} {RED}executable not found: {exe}{RESET}", flush=True)
        return None


def stop_all():
    """Stop all running processes."""
    global stopping
    stopping = True
    ts = timestamp()
    with lock:
        print(f"\n{GRAY}{ts}{RESET} {RED}{BOLD}[          stopping all]{RESET} {RED}sending termination signals...{RESET}", flush=True)

    for name, proc in processes:
        if proc.poll() is None:
            try:
                proc.terminate()
            except Exception:
                pass

    time.sleep(2)

    for name, proc in processes:
        if proc.poll() is None:
            try:
                proc.kill()
            except Exception:
                pass

    ts = timestamp()
    with lock:
        print(f"{GRAY}{ts}{RESET} {RED}{BOLD}[          stopped all]{RESET}", flush=True)


def monitor():
    """Monitor processes and report if any dies unexpectedly."""
    while not stopping:
        for name, proc in processes:
            if proc.poll() is not None and not stopping:
                ts = timestamp()
                with lock:
                    print(f"{GRAY}{ts}{RESET} {RED}{BOLD}[{name:>18s}]{RESET} {RED}exited with code {proc.returncode}{RESET}", flush=True)
        time.sleep(1)


def main():
    # Banner
    print(f"\n{BOLD}{'='*60}")
    print(f"  CipherLake Crypto Services Launcher")
    print(f"  {DIM}{len(SERVICES)} services | Ctrl+C to stop all{RESET}")
    print(f"{'='*60}{RESET}\n")

    # Ensure parent directories exist (NOT the key paths themselves - they are file paths)
    for d in ["./data/keys", "./data/keystore"]:
        os.makedirs(d, exist_ok=True)

    # Start all services
    for name, exe, args, color, env in SERVICES:
        start_service(name, exe, args, color, env)

    # Start monitor thread
    t_monitor = threading.Thread(target=monitor, daemon=True)
    t_monitor.start()

    # Handle Ctrl+C
    def on_signal(sig, frame):
        stop_all()
        sys.exit(0)

    signal.signal(signal.SIGINT, on_signal)
    if sys.platform != "win32":
        signal.signal(signal.SIGTERM, on_signal)

    # Wait for all processes
    try:
        for name, proc in processes:
            proc.wait()
    except KeyboardInterrupt:
        pass
    finally:
        stop_all()


if __name__ == "__main__":
    main()
