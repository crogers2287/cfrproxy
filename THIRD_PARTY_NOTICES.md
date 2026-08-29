# Third-party notices

## SIMURG

cfrproxy's output-integrity observer contains a modified, native Go
implementation of streaming corruption-detection ideas from
[SIMURG](https://github.com/doofzoff/SIMURG).

Copyright 2026 HAL-X AI / Farid Aghayev (doofZ).

SIMURG is licensed under the Apache License, Version 2.0. A copy is included
at [`licenses/SIMURG-APACHE-2.0.txt`](licenses/SIMURG-APACHE-2.0.txt).

The cfrproxy implementation is not a verbatim runtime embedding. It was
rewritten for incremental Go inference, uses cfrproxy-specific profile and
fusion rules, stores bounded observation checkpoints, and omits SIMURG's
Python runtime and learned calibration model. Modified source files carry an
attribution notice.
