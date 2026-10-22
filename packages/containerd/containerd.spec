Name:           containerd
Version:        %{upstream_version}
Release:        1.katl%{?dist}
Summary:        KatlOS container runtime and matching OCI shim
Vendor:         KatlOS
License:        Apache-2.0
URL:            https://containerd.io
Source0:        containerd-%{version}.tar.gz
Source1:        containerd-source.json
BuildRequires:  golang, gcc, binutils
Requires:       crun

%description
Containerd and its matching shim, built from the upstream release selected by KatlOS.

%prep
%setup -q
cp %{SOURCE1} katl-source.json

%build
export GOTOOLCHAIN=%{go_toolchain}
export GOFLAGS="-mod=vendor -trimpath -buildvcs=false"
export CGO_ENABLED=1
version_flags="-X github.com/containerd/containerd/v2/version.Version=v%{version} -X github.com/containerd/containerd/v2/version.Revision=%{upstream_revision} -X github.com/containerd/containerd/v2/version.Package=github.com/containerd/containerd/v2 -s -w"
for binary in containerd ctr containerd-shim-runc-v2; do
    tags=urfave_cli_no_docs
    if [ "$binary" = containerd-shim-runc-v2 ]; then tags="$tags no_grpc"; fi
    go build -buildmode=pie -tags "$tags" -ldflags "$version_flags" -o "bin/$binary" "./cmd/$binary"
done

%install
install -Dpm0755 bin/containerd %{buildroot}%{_bindir}/containerd
install -Dpm0755 bin/containerd-shim-runc-v2 %{buildroot}%{_bindir}/containerd-shim-runc-v2
install -Dpm0755 bin/ctr %{buildroot}%{_bindir}/ctr
sed 's|/usr/local/bin/containerd|%{_bindir}/containerd|' containerd.service > containerd-katl.service
install -Dpm0644 containerd-katl.service %{buildroot}%{_unitdir}/containerd.service

%check
bin/containerd --version | grep -F 'v%{version}'
bin/containerd-shim-runc-v2 -v | grep -F 'v%{version}'
bin/ctr --version | grep -F 'v%{version}'
for binary in containerd containerd-shim-runc-v2 ctr; do
    readelf -h "bin/$binary" | grep -E 'Type:.*DYN'
done

%files
%license LICENSE NOTICE
%doc katl-source.json
%{_bindir}/containerd
%{_bindir}/containerd-shim-runc-v2
%{_bindir}/ctr
%{_unitdir}/containerd.service
