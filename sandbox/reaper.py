"""PID 1 of the sandbox container: stays alive and reaps orphaned processes.

`sleep infinity` would never reap, so processes killed between tool calls
would linger as zombies and eat into the container's PID limit. As PID 1 this
process is also immune to the `kill -9 -1` that clears stragglers.
"""

import os
import time

while True:
    try:
        os.wait()
    except ChildProcessError:
        time.sleep(1)
