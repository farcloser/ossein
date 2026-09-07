just build
OSSEIN_KERNEL=v1-containerization/.src/kernel/kernel-arm64 \
OSSEIN_INITFS=v1-containerization/.src/bin/initfs.ext4 \
  ./build/ossein doctor

# M1: boot + vminitd handshake

export OSSEIN_KERNEL=v1-containerization/.src/kernel/kernel-arm64
export OSSEIN_INITFS=v1-containerization/.src/bin/initfs.ext4 \
  ./build/ossein run -it docker.io/library/alpine:3.22@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce /bin/sh

eval "$(./build/ossein buildkit)" && buildctl debug workers
