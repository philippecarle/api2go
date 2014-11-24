package api2go

import (
	"errors"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Errors test", func() {
	It("can create array tree", func() {
		err := NewHTTPError(errors.New("hi"), "hi", 0)
		httpErr, ok := err.(httpError)
		for i := 0; i < 20; i++ {
			httpErr.AddHTTPError(httpErr)
		}
		Expect(ok).To(Equal(true))
		Expect(httpErr.errorsCount).To(Equal(20))
		Expect(len(httpErr.errors)).To(Equal(httpErr.errorsCount))
	})
})
