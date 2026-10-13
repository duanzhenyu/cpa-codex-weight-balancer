# Toolchain for the Linux and Windows libraries. Darwin needs Apple's SDK and is
# built on a Mac; see dist.sh.
FROM golang:1.26.2-bookworm

# zig links the Linux libraries against glibc 2.17, the baseline CPA's own Linux
# release is built for. Linking against the image's glibc would demand 2.34.
ARG ZIG_VERSION=0.14.1
ARG ZIG_SHA256=24aeeec8af16c381934a6cd7d95c807a8cb2cf7df9fa40d359aa884195c4716c

RUN apt-get update \
	&& apt-get install -y --no-install-recommends gcc-mingw-w64-x86-64 xz-utils zip \
	&& rm -rf /var/lib/apt/lists/* \
	&& curl -fsSL -o /tmp/zig.tar.xz "https://ziglang.org/download/${ZIG_VERSION}/zig-x86_64-linux-${ZIG_VERSION}.tar.xz" \
	&& echo "${ZIG_SHA256}  /tmp/zig.tar.xz" | sha256sum -c - \
	&& tar -xJf /tmp/zig.tar.xz -C /opt \
	&& ln -s "/opt/zig-x86_64-linux-${ZIG_VERSION}/zig" /usr/local/bin/zig \
	&& rm /tmp/zig.tar.xz
