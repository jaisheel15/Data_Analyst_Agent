# Glossary

**Session**: A user's working space: the datasets they uploaded and the conversation about them. Holds many Runs.

**Run**: One user question and everything the agent does to answer it (model calls, tool calls, the final answer). Belongs to exactly one Session. Ends in one of four ways: done, budget exhausted, cancelled (the user went away), or error.

**Run purpose**: Why a Run happened: `chat` (a user asked) or `eval` (the eval harness asked).

**Tool call**: One request by the agent to run a tool (describe a dataset, run SQL, run Python) during a Run.

**Tool error**: A Tool call that ran, but whose SQL or Python was wrong (a syntax error, a missing column). A normal part of a Run: the model reads it and tries again. Not a fault.

**Tool failure**: A Tool call that could not do its job at all: the sandbox timed out or crashed, ran out of memory, or the tool service was unreachable. A fault. _Avoid_: calling this a "tool error".
