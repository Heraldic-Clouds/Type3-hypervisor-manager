# Source review and preservation

The existing repositories were inspected and clean branches were created before
implementation. No history was rewritten. The original Type3 main.go, LXD code
and existing mea-web Next.js source remain present.

- Type3 baseline: 573d2dc. Its old AWS SDK/LXD prototype, static credentials
  pattern and disabled test were unsuitable for the requested EC2 Newbie flow.
  A separate Go module preserves that prototype while providing the new API.
- mea-web baseline: 40e45c2. Its recursive-frame brand mark and navy/pink visual
  cues informed the new UI. The Next.js, Prisma and Stripe prototype is retained;
  the deployed Vite app is in newbie and implements only the free Newbie tier.
- guacamole-client checkout: 50cade5d16768f10502504e47d575f1b96cc9db0.
  The JSON authentication implementation, JDBC schema, extension event API,
  official browser library and Docker initialization code were reviewed.
  The exact official 1.6.0 JDBC schema was extracted from its 1.6.0 Git tag.
- guacamole-server checkout: 3844a5f70871d3abe4e0e8fc3977dbcd4d0cfb3b.
  This checkout is ahead of 1.6.0. The MVP uses the released official guacd
  1.6.0 image and RDP protocol, preserving local server work without introducing
  unreleased or GPU streaming changes into this deployment.

Official Guacamole and guacd base images are pinned by version and digest.
MEA's additional Java lifecycle code uses the documented extension API. It does
not permanently fork the upstream application, JDBC password hashing or tables.
The React application uses the official common-js from that exact image.

Backend branch: codex/newbie-mvp in Type3-hypervisor-manager.
Frontend branch: codex/newbie-mvp in mea-web.
