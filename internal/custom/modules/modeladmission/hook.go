package modeladmission

import "context"

type admissionGrantedHookKey struct{}

// WithAdmissionGrantedHook registers work that must run after distributed
// admission succeeds but before the provider request starts. Durable workers
// use this boundary to consume a provider-attempt only for a real call.
func WithAdmissionGrantedHook(
	ctx context.Context,
	hook func(context.Context) error,
) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if hook == nil {
		return ctx
	}
	return context.WithValue(ctx, admissionGrantedHookKey{}, hook)
}

func runAdmissionGrantedHook(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	hook, _ := ctx.Value(admissionGrantedHookKey{}).(func(context.Context) error)
	if hook == nil {
		return nil
	}
	return hook(ctx)
}
