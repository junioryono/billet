# pam-environment.awk: /etc/environment as pam_env reads it, one plain NAME=VALUE
# per line.
#
# THE ONE READER, used by the step wrapper in the image being built (each step
# starts in what earlier steps wrote, as Packer's sudo and ssh sessions do) and
# by the build for the environment the agent hands a job, so the two can never
# disagree about a line. What pam_env does with a line of /etc/environment
# (readenv=1, linux-pam's _parse_env_file): leading whitespace skipped, a line
# starting with # ignored, an `export ` prefix dropped, and one pair of matching
# quotes, single or double, around the whole value removed. Nothing is expanded.
# A line that is not NAME=VALUE after that is not an assignment, and is dropped.
{
	line = $0
	sub(/^[ \t]+/, "", line)
	if (line == "" || line ~ /^#/) {
		next
	}
	sub(/^export[ \t]+/, "", line)
	if (line !~ /^[A-Za-z_][A-Za-z0-9_]*=/) {
		next
	}
	eq = index(line, "=")
	name = substr(line, 1, eq - 1)
	value = substr(line, eq + 1)
	if (length(value) >= 2 && (value ~ /^".*"$/ || value ~ /^'.*'$/)) {
		value = substr(value, 2, length(value) - 2)
	}
	print name "=" value
}
