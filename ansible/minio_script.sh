#!/bin/bash

tee -a inventory << END
[master]
${1}
END

tee -a ansible.cfg << END
[defaults]
remote_user=${2}
inventory=inventory
roles_path=roles
host_key_checking=False
ask_pass=False
ansible_python_interpreter=/usr/bin/python3
deprecation_warnings=False
collections_paths=~/.ansible/collections/

[privilege_escalation]
become=True
become_method=sudo
become_user=root
become_ask_pass=False
END

tee -a input.yaml << END
BlockWorker: https://hero.0chain.net/dns
create_wallet: "no"
check_balance: "yes"
add_token: "yes"
register_wallet: "yes"
create_allocation: "no"
lock_tokens: 0.5          # pass lock onle when create_allocation: "yes"
minio_username: ${4}
minio_password: ${5}
config_dir: ${3}
allocation: ${6}  # uncomment only when create_allocation is "no" else comment it
END

ansible-playbook main.yaml -v > minio-detailed-output