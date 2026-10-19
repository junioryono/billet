# ee/

billet's enterprise features live here, under [the billet Enterprise License](LICENSE) rather than the Apache License 2.0 that covers the rest of the repository. You may read, modify and run this code for development and testing without a subscription; using it in production needs one.

It is empty for now. Single sign-on (SAML, SCIM and role mapping), roles and permissions, audit logs and policies are the features planned for it.

Two rules hold its boundary:

- **Nothing outside `ee/` imports anything inside it.** The Apache-licensed build is complete without this directory, and an enterprise feature is an addition to it, never a dependency of it.
- **Outside contributions need a written agreement** with the licensor, because the license keeps the rights to modifications with the licensor. Open an issue before writing code for this directory.
