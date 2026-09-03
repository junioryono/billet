.. toctree::
   :maxdepth: 2
   :caption: Getting Started
   :hidden:

   getting-started/index
   getting-started/installation
   getting-started/github-side
   getting-started/first-config
   getting-started/first-job
   getting-started/next-steps

.. toctree::
   :maxdepth: 2
   :caption: Concepts
   :hidden:

   concepts/how-it-works
   concepts/tiers-and-capacity
   concepts/trust-and-isolation
   concepts/sites-and-storage
   concepts/identity-and-security
   concepts/state-and-controllers

.. toctree::
   :maxdepth: 2
   :caption: Deploying
   :hidden:

   deploying/choose-a-shape
   deploying/single-host-docker
   deploying/linux-firecracker-host
   deploying/mac-tart
   deploying/aws-ec2
   deploying/aws-codebuild
   deploying/hybrid-owned-hardware
   deploying/postgres-and-active-passive
   deploying/reaching-hosts

.. toctree::
   :maxdepth: 2
   :caption: Operating
   :hidden:

   operating/status-and-leases
   operating/nodes
   operating/guest-images
   operating/actions-cache
   operating/compatibility
   operating/upgrades
   operating/backup-restore-recover
   operating/draining-and-stopping
   operating/ca-rotation
   operating/troubleshooting

.. toctree::
   :maxdepth: 2
   :caption: Reference
   :hidden:

   reference/cli
   reference/configuration
   reference/status
   reference/reference-hardware
   reference/action-versioning
   reference/upstream-references
   reference/decisions/index
   reference/records/index
   GitHub <https://github.com/junioryono/billet>
   API Docs <https://pkg.go.dev/github.com/junioryono/billet>
   Changelog <https://github.com/junioryono/billet/releases>

billet
======

**Self-hosted GitHub Actions runners on your own hardware, with the cloud as fallback and a cache beside the compute.**

billet runs your GitHub Actions jobs on machines you control: a server under your desk, a Mac mini, or EC2 when the box at home is off. One ``runs-on`` label can mean "the machine at home if it is up, the cloud if it is not". The control plane talks to GitHub over an outbound long-poll, so GitHub never connects to you, and a single-box deployment opens nothing at all.

.. code-block:: bash

   curl -fsSL https://raw.githubusercontent.com/junioryono/billet/main/scripts/install.sh | sh
   billet init --org your-org --runner-group billet-trusted \
     --workflow 'your-org/your-repo/.github/workflows/ci.yml@refs/heads/main' --config ~/billet.yaml
   billet github-app create --org your-org --config ~/billet.yaml
   billet check --config ~/billet.yaml
   billet server --config ~/billet.yaml   # and, in a second terminal:
   billet node   --config ~/billet.yaml

Then, in a workflow:

.. code-block:: yaml

   jobs:
     build:
       runs-on: billet-2vcpu

Where to start
--------------

- **New here?** `Get started <getting-started/index.html>`_ takes one Linux machine from nothing to a job in fifteen minutes.
- **Choosing a deployment?** `Choose a shape <deploying/choose-a-shape.html>`_ compares a single host, a Firecracker server, a Mac, AWS, and a hybrid of owned hardware with cloud fallback.
- **Running one already?** The pages under Operating cover status, nodes, images, upgrades, backup and recovery, drains, and CA rotation.
- **Looking something up?** `CLI <reference/cli.html>`_, `Configuration <reference/configuration.html>`_, `What is proven <reference/status.html>`_, and the architecture `decisions <reference/decisions/index.html>`_ and acceptance `records <reference/records/index.html>`_.

Status
------

billet is **pre-alpha**. Jobs run end to end through the Docker, Firecracker, Tart, EC2 and CodeBuild backends, and the same ``runs-on`` label has failed over from owned hardware to EC2 against real GitHub and AWS infrastructure. Do not point release or deploy pipelines at it yet. `What is proven <reference/status.html>`_ says exactly what has run for real, by backend, with dates.

License
-------

Apache-2.0. See `LICENSE <https://github.com/junioryono/billet/blob/main/LICENSE>`_.
