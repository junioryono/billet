// Package tfpolicy has no runtime code. Its test keeps the terraform-aws-billet
// module's committed IAM policy renderings equal to internal/awspolicy's
// generator, so the module can never grant a permission billet's own `init iam`
// would not.
package tfpolicy
