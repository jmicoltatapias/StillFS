# License and dependency review status

StillFS is prepared for the GNU AGPL version 3. The canonical license text is
in the repository's `LICENSE` file.

The selected PostgreSQL driver (`github.com/lib/pq`) includes MIT-licensed
code and BSD-3-Clause SCRAM portions in its upstream license file. The directly
declared frontend dependencies report MIT or Apache-2.0 in their upstream
metadata. The installed frontend tree's package metadata was inventoried;
licenses include MIT, Apache-2.0, ISC, BSD-3-Clause, and CC-BY-4.0. This is
only an initial check: license texts and conditions have not been individually
reviewed, nor have distribution obligations and compatibility with the
project's AGPL terms. This document is not legal advice or a completed legal
review.
