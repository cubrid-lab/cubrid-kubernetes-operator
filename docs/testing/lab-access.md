# Reaching the multi-VM lab

This page is for maintainers who hold lab credentials. Outside contributors do
not receive them, and nothing in this repository needs them (see
[CONTRIBUTING.md](../../CONTRIBUTING.md)).

The credentials, the node addresses and the full access guide live in the
lab's private provisioning repository; maintainers get the files from there,
encrypted to their GitHub SSH keys. This page lists only the steps and the
problems that come up, so they are found from this repository too. Never paste
an address, a key or a kubeconfig into an issue, a pull request or a workflow
here.

## What you have

- The node SSH key (`<node-key>.pem`) and the cluster's admin kubeconfig
  (`<kubeconfig>.yaml`).
- Usually also an `ssh_config` with one alias per node. Without it, connect by
  address as below.
- Your address must be registered: SSH (22) and the Kubernetes API (6443)
  accept only registered addresses. A timeout is never a key problem; check
  the address you leave from first (`curl -4 -s ifconfig.me`).

## Steps

1. Make both files private. SSH ignores a key that others can read:

   ```sh
   chmod 600 <node-key>.pem <kubeconfig>.yaml
   ```

2. Use the kubeconfig only for the command at hand, without changing your
   current context:

   ```sh
   KUBECONFIG=<kubeconfig>.yaml kubectl get nodes   # the server and three workers, Ready
   ```

   Do not leave your current context on the lab. The e2e suite's context
   check refuses it (`make test-e2e` runs only against its own Kind cluster),
   but other tools would not.

3. SSH to the server node as `ubuntu`; `root` login is disabled:

   ```sh
   ssh -i <node-key>.pem ubuntu@<server-address> hostname
   ```

4. Reach a worker by its alias from the `ssh_config`, or through the server.
   The `INTERNAL-IP` that `kubectl get nodes -o wide` shows is a private
   address and gives `No route to host` from outside:

   ```sh
   ssh -i <node-key>.pem -o IdentitiesOnly=yes \
     -o "ProxyCommand=ssh -i <node-key>.pem -o IdentitiesOnly=yes -W %h:%p ubuntu@<server-address>" \
     ubuntu@<worker-internal-ip> hostname
   ```

   `-J ubuntu@<server-address>` does not pass `-i` on to the jump, so it
   works only when the key is in `~/.ssh/config` or the SSH agent.
   `IdentitiesOnly=yes` keeps an agent with many keys from ending in
   `Too many authentication failures`.

5. From a script or an agent, SSH runs without a terminal (or with
   `-o BatchMode=yes`) and cannot answer the first-connection question. Pass
   `-o StrictHostKeyChecking=accept-new`: it records a host key seen for the
   first time and still refuses one that changed (OpenSSH 7.6 or later). For a
   jump, pass it to both commands, the outer `ssh` and the one in
   `ProxyCommand`: each checks its own host's key.

To run a command against the lab while holding it, so that no other campaign
interleaves, use `make test-lab` ([lab-runs.md](lab-runs.md)).

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `WARNING: UNPROTECTED PRIVATE KEY FILE!` or `Permission denied (publickey)` | Others can read the key, `-i` was not given, or it is the wrong key | `chmod 600 <node-key>.pem`, and pass it with `-i` |
| `Please login as the user "ubuntu" rather than the user "root".` | `root` login is disabled | Log in as `ubuntu`; use `sudo` |
| `No route to host` to a worker | The worker's `INTERNAL-IP` is not reachable from outside | Use the alias, or jump through the server (step 4) |
| `Host key verification failed` in a script | Without a terminal, SSH cannot answer the host-key question | Add `-o StrictHostKeyChecking=accept-new`, inside `ProxyCommand` too for a jump |
| `Identity file ... not accessible` with a jump host | The `ProxyCommand` was not quoted as one `-o` value | Quote the whole `-o "ProxyCommand=..."` |
| `ssh` or `kubectl` times out | Your address is not the registered one, or your network blocks 22 or 6443 | Compare `curl -4 -s ifconfig.me` with the registered address; try another network |
| kubectl `The connection to the server localhost:8080 was refused` | `KUBECONFIG` is not set for this command | Prefix the command with `KUBECONFIG=<kubeconfig>.yaml` |

Fault campaigns and other runs that disrupt the lab follow the reservation
rule in [CONTRIBUTING.md](../../CONTRIBUTING.md): one full lab run at a time.
