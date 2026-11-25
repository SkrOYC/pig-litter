# Structure

Pig Litter is an extension that the released PiG host loads for a root session. Its logical boundaries are modules inside one extension, so this file uses a module diagram instead of containers. One instance of these modules serves one root tree. A detached session is another PiG process with its own instance.

The [boundary records](boundaries.yaml) hold each module's responsibility, inputs, outputs, and edges.

```mermaid
flowchart TB
    Agent([Session agents])
    Operator([Operator])
    Peer([Local peer: PiG root or Claude Code session])
    Host([PiG host])
    Log[(Root conversation record)]
    Files[(Configuration files)]
    Pane([Terminal pane host])
    Detached([Detached PiG process])

    subgraph Tree["One root tree: one process group"]
        Ops["BND-01 Agent operations<br/>trust boundary"]
        Coord["BND-02 Tree coordinator"]
        Steer["BND-03 Steer delivery"]
        SHost["BND-04 Session host"]
        Record["BND-05 Tree record"]
        Gateway["BND-06 Peer gateway<br/>trust boundary"]
        Launcher["BND-07 Detached launcher"]
        View["BND-08 Operator view"]
        Config["BND-09 Settings and profiles"]
    end

    Agent --> Ops
    Operator --> View
    Ops -- in-process --> Coord
    Ops -- in-process --> Steer
    Ops -- in-process --> Gateway
    Ops -- in-process --> Launcher
    Coord -- in-process --> SHost
    SHost -- event --> Coord
    Coord -- in-process --> Steer
    Steer -- in-process --> Coord
    Steer -- in-process --> SHost
    Coord -- in-process --> Record
    Coord -- in-process --> Config
    Gateway -- in-process --> Steer
    Launcher -- in-process --> Record
    Launcher -- in-process --> Gateway
    Launcher -- in-process --> Config
    View -- in-process --> Coord
    View -- in-process --> Steer
    SHost -- event --> View
    SHost -- ipc --> Host
    Record -- shared-storage --> Log
    Gateway -- ipc --> Peer
    Launcher -- ipc --> Pane
    Launcher -- other: process start and end --> Detached
    Config -- file-handoff --> Files
```

## Reading the diagram

- **One tree is one failure unit.** The root, the extension, and every dependent child end together. Only the tree record and each session's stored conversation outlive the process.
- **Two trust boundaries.** Agent operations decides what an agent may do to another session. The peer gateway is where input from outside the tree arrives.
- **One way in for content.** Every input that reaches a session's conversation passes through steer delivery, which labels it with its origin.
- **One seam for running sessions.** Only the session host creates, drives, and observes sessions on the released PiG host. The tree record uses the host's conversation storage, and the operator view uses the host's drawing surface. The technical specification decides whether those two uses go through the same seam.
- **Dependent children add no boundary.** A child at any depth calls the same agent operations and is hosted by the same session host.
