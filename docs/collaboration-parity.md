# Collaboration contract

The self-contained catalog at [requirements.json](../scripts/testdata/collaboration/requirements.json) defines Pig Litter's collaboration requirements and acceptance scenarios.

Each requirement names its behavior, owning milestone, acceptance scenario, and evidence state. Its reference describes the project requirement, using the requirement ID as its section. No external research files are required.

The verification tools validate catalog structure and collect product evidence. A planned scenario is not a passing result. Evidence applies only to the platform, architecture, and behavior actually verified.

Run the catalog checks with `direnv exec . go -C scripts test ./cmd/verify-lifecycle -run CollaborationCatalog`.
