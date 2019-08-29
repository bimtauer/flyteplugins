package k8sarray

import (
	"context"

	"github.com/lyft/flyteplugins/go/tasks/v1/array/arraystatus"

	"strconv"

	structpb "github.com/golang/protobuf/ptypes/struct"
	idlPlugins "github.com/lyft/flyteidl/gen/pb-go/flyteidl/plugins"
	"github.com/lyft/flyteplugins/go/tasks/pluginmachinery/v1/core"
	"github.com/lyft/flyteplugins/go/tasks/pluginmachinery/v1/io"
	"github.com/lyft/flyteplugins/go/tasks/pluginmachinery/v1/ioutils"
	"github.com/lyft/flyteplugins/go/tasks/pluginmachinery/v1/utils"
	"github.com/lyft/flyteplugins/go/tasks/pluginmachinery/v1/workqueue"
	"github.com/lyft/flyteplugins/go/tasks/v1/errors"
	"github.com/lyft/flyteplugins/go/tasks/v1/flytek8s"
	"github.com/lyft/flytestdlib/logger"
	"github.com/lyft/flytestdlib/storage"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const K8sPodKind = "pod"

type Phase uint8

const (
	PhaseNotStarted Phase = iota
	PhaseSubmittedToCatalogReader
	PhaseMappingFileCreated
	PhaseJobSubmitted
	PhaseJobsFinished
)

type State struct {
	currentPhase    Phase
	actualArraySize int
	arrayStatus     arraystatus.ArrayStatus
}

func (s State) GetActualArraySize() int {
	return s.actualArraySize
}

func (s State) GetPhase() Phase {
	return s.currentPhase
}

func (s State) GetArrayStatus() arraystatus.ArrayStatus {
	return s.arrayStatus
}

func (s *State) SetActualArraySize(size int) *State {
	s.actualArraySize = size
	return s
}

func (s *State) SetPhase(newPhase Phase) *State {
	s.currentPhase = newPhase
	return s
}

func (s *State) SetArrayStatus(state arraystatus.ArrayStatus) *State {
	s.arrayStatus = state
	return s
}

/*
  Discovery for sub-tasks
  Build mapping file
---
  submit jobs (either as a batch or individually)
---BestEffort
  Detect changes to individual job states
    - Check failure ratios
---
  Submit to discovery

*/

func ToArrayJob(structObj *structpb.Struct) (*idlPlugins.ArrayJob, error) {
	arrayJob := &idlPlugins.ArrayJob{}
	err := utils.UnmarshalStruct(structObj, arrayJob)
	return arrayJob, err
}

// Check if there are any previously cached tasks. If there are we will only submit an ArrayJob for the
// non-cached tasks. The ArrayJob is now a different size, and each task will get a new index location
// which is different than their original location. To find the original index we construct an indexLookup array.
// The subtask can find it's original index value in indexLookup[JOB_ARRAY_INDEX] where JOB_ARRAY_INDEX is an
// environment variable in the pod
func DetermineDiscoverability(ctx context.Context, tCtx core.TaskExecutionContext, state State,
	catalogReader workqueue.IndexedWorkQueue) (State, error) {

	// Check that the taskTemplate is valid
	taskTemplate, err := tCtx.TaskReader().Read(ctx)
	if err != nil {
		return state, err
	} else if taskTemplate == nil {
		return state, errors.Errorf(errors.BadTaskSpecification, "Required value not set, taskTemplate is nil")
	}

	// Extract the custom plugin pb
	arrayJob, err := ToArrayJob(taskTemplate.GetCustom())
	if err != nil {
		return state, err
	} else if arrayJob == nil {
		return state, errors.Errorf(errors.BadTaskSpecification, "Could not extract custom array job")
	}

	// If discoverable, then submit all the tasks to the data catalog worker
	if taskTemplate.Metadata != nil && taskTemplate.Metadata.Discoverable {
		// build input readers
		err = ConstructInputReaders(ctx, tCtx.DataStore(), tCtx.InputReader().GetInputPrefixPath(), int(arrayJob.Size))
		// build output writers
		err = ConstructOutputWriters(ctx, tCtx.DataStore(), tCtx.OutputWriter().GetOutputPrefixPath(), int(arrayJob.Size))

		// build work items
		// enqueue
		// check work item status
		//  if all done, write mapping file, then build updated state (new size, new phase)
	} else {
		logger.Infof(ctx, "Task is not discoverable, moving to launch phase...")
	}

	return state, nil
}

type Blah struct {
	io.OutputWriter
	loc storage.DataReference
}

func ConstructInputReaders(ctx context.Context, dataStore *storage.DataStore, inputPrefix storage.DataReference,
	size int) error {

	// Turn into input reader
	arrayInputPaths := make([]storage.DataReference, size)

	for i := 0; i < int(size); i++ {
		dataReference, err := GetPath(ctx, dataStore, inputPrefix, strconv.Itoa(i))
		if err != nil {
			return err
		}
		arrayInputPaths = append(arrayInputPaths, dataReference)
	}
	return nil
}

func ConstructOutputWriters(ctx context.Context, dataStore *storage.DataStore, outputPrefix storage.DataReference, size int) error {

	// turn into output writer
	arrayInputPaths := make([]storage.DataReference, size)

	for i := 0; i < int(size); i++ {
		dataReference, err := GetPath(ctx, dataStore, outputPrefix, strconv.Itoa(i))
		if err != nil {
			return err
		}
		ioutils.NewSimpleOutputReader()
		arrayInputPaths = append(arrayInputPaths, dataReference)
	}
	return nil

}

// Note that Name is not set on the result object.
// It's up to the caller to set the Name before creating the object in K8s.
func FlyteArrayJobToK8sPodTemplate(ctx context.Context, tCtx core.TaskExecutionContext) (
	podTemplate v1.Pod, job *idlPlugins.ArrayJob, err error) {

	// Check that the taskTemplate is valid
	taskTemplate, err := tCtx.TaskReader().Read(ctx)
	if err != nil {
		return v1.Pod{}, nil, err
	} else if taskTemplate == nil {
		return v1.Pod{}, nil, errors.Errorf(errors.BadTaskSpecification, "Required value not set, taskTemplate is nil")
	}

	if taskTemplate.GetContainer() == nil {
		return v1.Pod{}, nil, errors.Errorf(errors.BadTaskSpecification,
			"Required value not set, taskTemplate Container")
	}

	var arrayJob *idlPlugins.ArrayJob
	if taskTemplate.GetCustom() != nil {
		arrayJob, err = ToArrayJob(taskTemplate.GetCustom())
		if err != nil {
			return v1.Pod{}, nil, err
		}
	}

	podSpec, err := flytek8s.ToK8sPodSpec(ctx, tCtx.TaskExecutionMetadata(), tCtx.TaskReader(), tCtx.InputReader(),
		tCtx.OutputWriter().GetOutputPrefixPath().String())
	if err != nil {
		return v1.Pod{}, nil, err
	}

	// TODO: confirm whether this can be done when creating the pod spec directly above
	podSpec.Containers[0].Command = taskTemplate.GetContainer().Command
	podSpec.Containers[0].Args = taskTemplate.GetContainer().Args

	return v1.Pod{
		TypeMeta: metav1.TypeMeta{
			Kind:       K8sPodKind,
			APIVersion: v1.SchemeGroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			// Note that name is missing here
			Namespace:       tCtx.TaskExecutionMetadata().GetNamespace(),
			Labels:          tCtx.TaskExecutionMetadata().GetLabels(),
			Annotations:     tCtx.TaskExecutionMetadata().GetAnnotations(),
			OwnerReferences: []metav1.OwnerReference{tCtx.TaskExecutionMetadata().GetOwnerReference()},
		},
		Spec: *podSpec,
	}, arrayJob, nil
}