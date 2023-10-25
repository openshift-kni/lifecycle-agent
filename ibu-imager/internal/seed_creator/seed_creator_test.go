package seed_creator

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/sirupsen/logrus"
	"ibu-imager/internal/ops"
)

func TestIbuImager(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "IbuImager Suite")
}

var _ = Describe("Backup /var", func() {
	var (
		l       = logrus.New()
		ctrl    *gomock.Controller
		opsMock *ops.MockOps
		seed    *SeedCreator
		tmpDir  string
	)

	BeforeEach(func() {
		ctrl = gomock.NewController(GinkgoT())
		opsMock = ops.NewMockOps(ctrl)
		tmpDir, _ = os.MkdirTemp("", "test")
		seed = NewSeedCreator(l, opsMock, nil, tmpDir, "", "", "", "")
	})
	AfterEach(func() {
		Expect(os.RemoveAll(tmpDir)).To(Succeed())
	})

	It("Full flow", func() {
		args := []string{"czf", path.Join(tmpDir, "var.tgz"), "--exclude", "'/var/tmp/*'",
			"--exclude", "'/var/lib/log/*'", "--exclude", "'/var/log/*'", "--exclude", "'/var/lib/containers/*'", "--exclude",
			"'/var/lib/kubelet/pods/*'", "--exclude", "'/var/lib/cni/bin/*'", "--selinux", "/var"}
		opsMock.EXPECT().RunBashInHostNamespace("tar", args).Times(1).Return("", nil)
		err := seed.backupVar()
		Expect(err).ToNot(HaveOccurred())
	})

	It("var.tgz was already created, no need to run", func() {
		newPath := filepath.Join(tmpDir, "var.tgz")
		f, err := os.Create(newPath)
		Expect(err).ToNot(HaveOccurred())
		_ = f.Close()
		err = seed.backupVar()
		Expect(err).ToNot(HaveOccurred())
	})

	It("Fail to run tar command", func() {
		opsMock.EXPECT().RunBashInHostNamespace("tar", gomock.Any()).Times(1).Return("", fmt.Errorf("Dummy"))
		err := seed.backupVar()
		Expect(err).To(HaveOccurred())
	})
})
