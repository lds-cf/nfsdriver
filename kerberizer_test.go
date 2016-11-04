package nfsdriver_test

import (
	"errors"

	"code.cloudfoundry.org/goshims/execshim/exec_fake"
	"code.cloudfoundry.org/lager/lagertest"
	"github.com/lds-cf/nfsdriver"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("kerberizer", func() {
	var (
		subject    nfsdriver.Kerberizer
		testLogger = lagertest.NewTestLogger("kerberizer")
		fakeExec   *exec_fake.FakeExec
		fakeCmd    *exec_fake.FakeCmd

		err error
	)
	const principal = "testPrincipal"
	const credential = "testCredential"

	BeforeEach(func() {
		fakeCmd = &exec_fake.FakeCmd{}
		fakeExec = &exec_fake.FakeExec{}

		fakeExec.CommandReturns(fakeCmd)
		subject = nfsdriver.NewKerberizer(principal, credential, fakeExec)
	})

	Context("credentials valid", func() {
		BeforeEach(func() {
			err = subject.Login(testLogger)

		})

		It("should be able to login", func() {
			Expect(err).NotTo(HaveOccurred())
		})
	})
	Context("credentials invalid", func() {
		BeforeEach(func() {
			fakeCmd.RunReturns(errors.New("badness"))
			err = subject.Login(testLogger)

		})

		It("should NOT be able to login", func() {
			Expect(err).To(HaveOccurred())
		})
	})
})