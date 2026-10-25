# Mise En Abyme Newbie MVP

The current deployed cloud desktop service is in [mvp](mvp/DEPLOY.md). It uses
Go Lambda, PostgreSQL, Guacamole on Fargate and disposable Ubuntu EC2 desktops.
Frontend: the sibling mea-web repository's newbie directory. Deployment and
local development instructions, tests and cost information are in mvp.

The original Type3 prototype below is retained for history and is not deployed.

Type3 - Remote LXD server manager
==================================================

Type3 is an open source manager of LXD container hosts. Container servers act as a new type of virtualization (different from Type 1 or Type 2 hypervisors), and Type3 is a remote administration tool for LXD hosts.

Features
-----------

Features planned include:

* Deployment of containers in a way similar to deployment of VMs
* Management of container lifesycles (create, start, stop, delete)
* Container resource management (CPU, RAM, Storage, GPU)
* Container storage pool management
* Container image management
* Container network management 
* Contaiter snapshoting and migration management


How to use it?
------------------

This program isdesigned run serverlessly in the cloud. IMPORTANT: Type3 needs to be able to reach LXD servers on port 8443 (LXD default API port).
The program can will consist of fuctions (like CreateContainer, StartContainer, StopContainer, DeleteContainer) and each will take in parameters (name of the container, base image, instance flavor). You will need to write another program that calls Type3 and passes the name of the function with a list of parameters. If the project receives enough attention from the community a GUI will be developed with wizards and menus similar to other hypervisor remote management software.

Type3 uses the following dependencies:
------------------

* LXD Go API by Canonical
* AWS SDK for GO
