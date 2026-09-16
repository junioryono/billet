#!/usr/bin/python
# Copyright (c) 2026 junioryono
# Apache-2.0

"""Prove the retained account against the protected installer record."""

DOCUMENTATION = r'''
---
module: retired_account
short_description: Observe a retained service account without changing it
description:
  - Reads the fixed service-account record through a bounded, protected descriptor.
  - Compares its names and numeric IDs with the local account database.
options:
  user:
    description: Requested service account name.
    type: str
    required: true
  group:
    description: Requested service group name.
    type: str
    required: true
  home:
    description: Requested account home, which must remain the state root.
    type: path
    required: true
author: [junioryono]
'''
EXAMPLES = r'''
- name: Verify the retained account
  junioryono.billet.retired_account:
    user: billet
    group: billet
    home: /var/lib/billet
'''
RETURN = r'''
verified:
  description: The existing names, IDs and home match the protected record.
  type: bool
  returned: success
'''

import grp
import json
import os
import pwd
import stat

from ansible.module_utils.basic import AnsibleModule


def unique(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError('repeated account field')
        result[key] = value
    return result


def verify(user, group, home, path='/var/lib/billet/service-account'):
    identity = os.open(path, os.O_PATH | os.O_NOFOLLOW | os.O_CLOEXEC)
    try:
        info = os.fstat(identity)
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o022 or info.st_nlink != 1 or info.st_size > 4096:
            raise ValueError('untrusted account record')
        fd = os.open('/proc/self/fd/%d' % identity, os.O_RDONLY | os.O_NONBLOCK | os.O_CLOEXEC)
        with os.fdopen(fd, 'rb') as stream:
            body = stream.read(4097)
        if len(body) > 4096:
            raise ValueError('account record exceeds its bound')
        account = json.loads(body, object_pairs_hook=unique)
        if set(account) != {'user', 'group', 'uid', 'gid'} or account['user'] != user or account['group'] != group or home != '/var/lib/billet':
            raise ValueError('account names or home differ')
        for key in ['uid', 'gid']:
            if type(account[key]) is not int or not 0 < account[key] < 2**32 - 1:
                raise ValueError('account ID is unsupported')
        usr, grp_entry = pwd.getpwnam(user), grp.getgrnam(group)
        if (usr.pw_name, usr.pw_uid, usr.pw_gid, usr.pw_dir, grp_entry.gr_name, grp_entry.gr_gid) != (user, account['uid'], account['gid'], home, group, account['gid']):
            raise ValueError('local account differs from record')
    finally:
        os.close(identity)


def main():
    module = AnsibleModule(argument_spec=dict(user=dict(type='str', required=True), group=dict(type='str', required=True), home=dict(type='path', required=True)), supports_check_mode=True)
    try:
        verify(**module.params)
    except (OSError, ValueError, KeyError, TypeError):
        module.fail_json(msg='The protected retained service-account record and existing local account could not be proved equal.')
    module.exit_json(changed=False, verified=True)


if __name__ == '__main__':
    main()
