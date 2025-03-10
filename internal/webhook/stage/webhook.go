package stage

import (
	"context"
	"fmt"

	"github.com/Masterminds/semver"
	"github.com/argoproj-labs/argocd-image-updater/pkg/image"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kargoapi "github.com/akuity/kargo/api/v1alpha1"
	"github.com/akuity/kargo/internal/api/validation"
)

var (
	stageGroupKind = schema.GroupKind{
		Group: kargoapi.GroupVersion.Group,
		Kind:  "Stage",
	}
)

type webhook struct {
	client client.Client
}

func SetupWebhookWithManager(mgr ctrl.Manager) error {
	w := &webhook{
		client: mgr.GetClient(),
	}
	return ctrl.NewWebhookManagedBy(mgr).
		For(&kargoapi.Stage{}).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

func (w *webhook) Default(_ context.Context, _ runtime.Object) error {
	// Note that defaults are applied BEFORE validation, so we do not have the
	// luxury of assuming certain required fields must be non-nil.
	return nil
}

func (w *webhook) ValidateCreate(ctx context.Context, obj runtime.Object) error {
	stage := obj.(*kargoapi.Stage) // nolint: forcetypeassert
	if err := w.validateProject(ctx, stage); err != nil {
		return err
	}
	return w.validateCreateOrUpdate(stage)
}

func (w *webhook) ValidateUpdate(
	ctx context.Context,
	_ runtime.Object,
	newObj runtime.Object,
) error {
	stage := newObj.(*kargoapi.Stage) // nolint: forcetypeassert
	if err := w.validateProject(ctx, stage); err != nil {
		return err
	}
	return w.validateCreateOrUpdate(stage)
}

func (w *webhook) ValidateDelete(ctx context.Context, obj runtime.Object) error {
	stage := obj.(*kargoapi.Stage) // nolint: forcetypeassert
	return w.validateProject(ctx, stage)
}

func (w *webhook) validateProject(ctx context.Context, stage *kargoapi.Stage) error {
	if err := validation.ValidateProject(ctx, w.client, stage.GetNamespace()); err != nil {
		if errors.Is(err, validation.ErrProjectNotFound) {
			return apierrors.NewNotFound(schema.GroupResource{
				Group:    corev1.SchemeGroupVersion.Group,
				Resource: "Namespace",
			}, stage.GetNamespace())
		}
		var fieldErr *field.Error
		if ok := errors.As(err, &fieldErr); ok {
			return apierrors.NewInvalid(stageGroupKind, stage.GetName(), field.ErrorList{fieldErr})
		}
		return apierrors.NewInternalError(err)
	}
	return nil
}

func (w *webhook) validateCreateOrUpdate(e *kargoapi.Stage) error {
	if errs := w.validateSpec(field.NewPath("spec"), e.Spec); len(errs) > 0 {
		return apierrors.NewInvalid(stageGroupKind, e.Name, errs)
	}
	return nil
}

func (w *webhook) validateSpec(
	f *field.Path,
	spec *kargoapi.StageSpec,
) field.ErrorList {
	if spec == nil { // nil spec is caught by declarative validations
		return nil
	}
	errs := w.validateSubs(f.Child("subscriptions"), spec.Subscriptions)
	return append(
		errs,
		w.validatePromotionMechanisms(
			f.Child("promotionMechanisms"),
			spec.PromotionMechanisms)...,
	)
}

func (w *webhook) validateSubs(
	f *field.Path,
	subs *kargoapi.Subscriptions,
) field.ErrorList {
	if subs == nil { // nil subs is caught by declarative validations
		return nil
	}
	// Can subscribe to repos XOR upstream Stages
	if (subs.Repos == nil && len(subs.UpstreamStages) == 0) ||
		(subs.Repos != nil && len(subs.UpstreamStages) > 0) {
		return field.ErrorList{
			field.Invalid(
				f,
				subs,
				fmt.Sprintf(
					"exactly one of %s.repos or %s.upstreamStages must be defined",
					f.String(),
					f.String(),
				),
			),
		}
	}
	return w.validateRepoSubs(f.Child("repos"), subs.Repos)
}

func (w *webhook) validateRepoSubs(
	f *field.Path,
	subs *kargoapi.RepoSubscriptions,
) field.ErrorList {
	if subs == nil {
		return nil
	}
	// Must subscribe to at least one repo of some sort
	if len(subs.Git) == 0 && len(subs.Images) == 0 && len(subs.Charts) == 0 {
		return field.ErrorList{
			field.Invalid(
				f,
				subs,
				fmt.Sprintf(
					"at least one of %s.git, %s.images, or %s.charts must be non-empty",
					f.String(),
					f.String(),
					f.String(),
				),
			),
		}
	}
	errs := w.validateImageSubs(f.Child("images"), subs.Images)
	return append(errs, w.validateChartSubs(f.Child("charts"), subs.Charts)...)
}

func (w *webhook) validateImageSubs(
	f *field.Path,
	subs []kargoapi.ImageSubscription,
) field.ErrorList {
	var errs field.ErrorList
	for i, sub := range subs {
		errs = append(errs, w.validateImageSub(f.Index(i), sub)...)
	}
	return errs
}

func (w *webhook) validateImageSub(
	f *field.Path,
	sub kargoapi.ImageSubscription,
) field.ErrorList {
	var errs field.ErrorList
	if err := validateSemverConstraint(
		f.Child("semverConstraint"),
		sub.SemverConstraint,
	); err != nil {
		errs = field.ErrorList{err}
	}
	if sub.Platform != "" {
		if _, _, _, err := image.ParsePlatform(sub.Platform); err != nil {
			errs = append(errs, field.Invalid(f.Child("platform"), sub.Platform, ""))
		}
	}
	return errs
}

func (w *webhook) validateChartSubs(
	f *field.Path,
	subs []kargoapi.ChartSubscription,
) field.ErrorList {
	var errs field.ErrorList
	for i, sub := range subs {
		errs = append(errs, w.validateChartSub(f.Index(i), sub)...)
	}
	return errs
}

func (w *webhook) validateChartSub(
	f *field.Path,
	sub kargoapi.ChartSubscription,
) field.ErrorList {
	if err := validateSemverConstraint(
		f.Child("semverConstraint"),
		sub.SemverConstraint,
	); err != nil {
		return field.ErrorList{err}
	}
	return nil
}

func (w *webhook) validatePromotionMechanisms(
	f *field.Path,
	promoMechs *kargoapi.PromotionMechanisms,
) field.ErrorList {
	if promoMechs == nil { // nil promoMechs is caught by declarative validations
		return nil
	}
	// Must define at least one mechanism
	if len(promoMechs.GitRepoUpdates) == 0 &&
		len(promoMechs.ArgoCDAppUpdates) == 0 {
		return field.ErrorList{
			field.Invalid(
				f,
				promoMechs,
				fmt.Sprintf(
					"at least one of %s.gitRepoUpdates or %s.argoCDAppUpdates must "+
						"be non-empty",
					f.String(),
					f.String(),
				),
			),
		}
	}
	return w.validateGitRepoUpdates(
		f.Child("gitRepoUpdates"),
		promoMechs.GitRepoUpdates,
	)
}

func (w *webhook) validateGitRepoUpdates(
	f *field.Path,
	updates []kargoapi.GitRepoUpdate,
) field.ErrorList {
	var errs field.ErrorList
	for i, update := range updates {
		errs = append(errs, w.validateGitRepoUpdate(f.Index(i), update)...)
	}
	return errs
}

func (w *webhook) validateGitRepoUpdate(
	f *field.Path,
	update kargoapi.GitRepoUpdate,
) field.ErrorList {
	var count int
	if update.Bookkeeper != nil {
		count++
	}
	if update.Kustomize != nil {
		count++
	}
	if update.Helm != nil {
		count++
	}
	if count > 1 {
		return field.ErrorList{
			field.Invalid(
				f,
				update,
				fmt.Sprintf(
					"no more than one of %s.bookkeeper, or %s.kustomize, or %s.helm may "+
						"be defined",
					f.String(),
					f.String(),
					f.String(),
				),
			),
		}
	}
	return w.validateHelmPromotionMechanism(f.Child("helm"), update.Helm)
}

func (w *webhook) validateHelmPromotionMechanism(
	f *field.Path,
	promoMech *kargoapi.HelmPromotionMechanism,
) field.ErrorList {
	if promoMech == nil {
		return nil
	}
	// This mechanism must define at least one change to apply
	if len(promoMech.Images) == 0 && len(promoMech.Charts) == 0 {
		return field.ErrorList{
			field.Invalid(
				f,
				promoMech,
				fmt.Sprintf(
					"at least one of %s.images or %s.charts must be non-empty",
					f.String(),
					f.String(),
				),
			),
		}
	}
	return nil
}

func validateSemverConstraint(
	f *field.Path,
	semverConstraint string,
) *field.Error {
	if semverConstraint == "" {
		return nil
	}
	if _, err := semver.NewConstraint(semverConstraint); err != nil {
		return field.Invalid(f, semverConstraint, "")
	}
	return nil
}
