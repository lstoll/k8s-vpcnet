Vagrant.configure("2") do |config|
  config.vm.box = "ubuntu/xenial64"

  config.vm.provider "virtualbox" do |v|
    v.memory = 2048
    v.cpus = 2
  end

  config.vm.synced_folder ".", "/go/src/github.com/lstoll/k8s-vpcnet"

  config.vm.provision "shell", inline: <<-SHELL
if [ ! -d /usr/local/go ]; then
  curl -sL https://storage.googleapis.com/golang/go1.8.3.linux-amd64.tar.gz | tar -zx -C /usr/local
  echo 'export PATH=/usr/local/go/bin:$PATH' > /etc/profile.d/go.sh
  echo 'export GOPATH=/go' >> /etc/profile.d/go.sh
fi
if [ ! -f /usr/bin/gcc ]; then
  apt-get update
  apt-get -q -y install build-essential
fi
chown ubuntu /go
mkdir -p /go/pkg && chown ubuntu /go/pkg
SHELL
end
