#!/usr/bin/env python3
"""Reproduce only the blocked container-start precondition, without credentials."""
import datetime
import json
from pathlib import Path
import shlex
import signal
import subprocess
import sys
import uuid

IMAGE = 'sha256:5db6422a44cadc44857ac5c112f9d3634630cd78ea37dbe80a72d84571598a70'
nonce = uuid.uuid4().hex
name = 'prestart-' + nonce[:10] + '-0'
network = name + '-net'
label = 'poc.ha-password-prestart.run=' + nonce
out = Path(__file__).resolve().parent / ('reproduction-' + nonce[:10])
out.mkdir()
records = []
command_timed_out = False
network_owned = False
cleanup_ok = True
result = {'result': 'blocked', 'reason': 'container precondition not reached'}


def command(args, timeout=90):
    global command_timed_out
    start = datetime.datetime.now(datetime.timezone.utc).isoformat()
    try:
        completed = subprocess.run(args, text=True, stdout=subprocess.PIPE,
                                   stderr=subprocess.STDOUT, timeout=timeout)
        rc, output = completed.returncode, completed.stdout
    except subprocess.TimeoutExpired as error:
        command_timed_out = True
        raw = error.stdout or b''
        output = raw.decode(errors='replace') if isinstance(raw, bytes) else raw
        rc, output = 124, output + '\nHOST COMMAND DEADLINE\n'
    records.append({'startUTC': start, 'endUTC': datetime.datetime.now(datetime.timezone.utc).isoformat(),
                    'command': shlex.join(args), 'exit': rc, 'output': output})
    (out / 'commands.json').write_text(json.dumps(records, indent=2) + '\n')
    if rc:
        raise RuntimeError('command failed with exit ' + str(rc))
    return output.strip()


def interrupted(signum, frame):
    raise RuntimeError('interrupted by signal ' + str(signum))


signal.signal(signal.SIGINT, interrupted)
signal.signal(signal.SIGTERM, interrupted)
try:
    command(['docker', 'image', 'inspect', IMAGE, '--format', '{{json .Id}} {{json .RepoDigests}}'])
    command(['docker', 'network', 'create', '--label', label, network])
    network_owned = True
    command(['docker', 'run', '--pull', 'never', '-d', '--init', '--name', name, '--hostname', name,
             '--network', network, '--label', label, '--memory', '512m', '--cpus', '0.5',
             '--entrypoint', 'sleep', IMAGE, 'infinity'])
    result = {'result': 'precondition-only', 'reason': 'container starts; no engine or password check performed'}
except RuntimeError as error:
    result = {'result': 'blocked', 'reason': str(error)}
finally:
    signal.signal(signal.SIGINT, signal.SIG_IGN)
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
    # Docker can finish creating after a CLI timeout. Delete by returned ID
    # only when the exact, freshly generated run label proves ownership.
    try:
        check = subprocess.run(['docker', 'container', 'inspect', name], text=True,
                               stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=15)
    except subprocess.TimeoutExpired:
        check = None
        cleanup_ok = False
    if check is not None and check.returncode == 0:
        resource = json.loads(check.stdout)[0]
        if resource['Config'].get('Labels', {}).get('poc.ha-password-prestart.run') == nonce:
            try:
                command(['docker', 'rm', '-f', '-v', resource['Id']], timeout=30)
            except RuntimeError:
                cleanup_ok = False
        else:
            cleanup_ok = False
    if network_owned:
        try:
            command(['docker', 'network', 'rm', network], timeout=30)
        except RuntimeError:
            cleanup_ok = False
    result['cleanupSucceeded'] = cleanup_ok and not command_timed_out
    result['runLabel'] = label
    if command_timed_out:
        result['cleanupLimitation'] = 'A daemon request may complete after CLI cancellation; an immediate empty inventory cannot prove settlement. Reinspect only this run label before final cleanup.'
    (out / 'result.json').write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps(result))
sys.exit(2 if result['result'] == 'blocked' or not result['cleanupSucceeded'] else 0)
