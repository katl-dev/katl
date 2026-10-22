Name:           containerd
Version:        %{upstream_version}
Release:        1.katl%{?dist}
Summary:        KatlOS container runtime and matching OCI shim
Vendor:         KatlOS
License:        Apache-2.0
URL:            https://containerd.io
Source0:        containerd-%{version}.tar.gz
Source1:        containerd-source.json
BuildRequires:  golang, gcc, make
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
make %{?_smp_mflags} VERSION=v%{version} REVISION=%{upstream_revision} PACKAGES= API_PACKAGES= \
    bin/containerd bin/containerd-shim-runc-v2 bin/ctr

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

%files
%license LICENSE NOTICE
%doc katl-source.json
%{_bindir}/containerd
%{_bindir}/containerd-shim-runc-v2
%{_bindir}/ctr
%{_unitdir}/containerd.service
