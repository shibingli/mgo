package mgo

import (
	"fmt"
	"reflect"
	"sync"

	"github.com/globalsign/mgo/bson"
)

type ChangeStream struct {
	iter           *Iter
	options        ChangeStreamOptions
	pipeline       interface{}
	resumeToken    *bson.Raw
	collection     *Collection
	readPreference *ReadPreference
	err            error
	m              sync.Mutex
}

type ChangeStreamOptions struct {

	// FullDocument controls the amount of data that the server will return when
	// returning a changes document.
	FullDocument string

	// ResumeAfter specifies the logical starting point for the new change stream.
	ResumeAfter *bson.Raw

	// MaxAwaitTimeMS specifies the maximum amount of time for the server to wait
	// on new documents to satisfy a change stream query.
	MaxAwaitTimeMS int64

	// BatchSize specifies the number of documents to return per batch.
	BatchSize int32

	// Collation specifies the way the server should collate returned data.
	Collation *Collation
}

// Watch constructs a new ChangeStream capable of receiving continuing data
// from the database.
func (coll *Collection) Watch(pipeline interface{},
	options ChangeStreamOptions) (*ChangeStream, error) {

	if pipeline == nil {
		pipeline = []bson.M{}
	}

	pipe := constructChangeStreamPipeline(pipeline, options)

	pIter := coll.Pipe(&pipe).Iter()

	// check that there was no issue creating the iterator.
	// this will fail immediately with an error from the server if running against
	// a standalone.
	if err := pIter.Err(); err != nil {
		return nil, err
	}

	pIter.isChangeStream = true

	return &ChangeStream{
		iter:        pIter,
		collection:  coll,
		resumeToken: nil,
		options:     options,
		pipeline:    pipeline,
	}, nil
}

// Next retrieves the next document from the change stream, blocking if necessary.
// Next returns true if a document was successfully unmarshalled into result,
// and false if an error occured. When Next returns false, the Err method should
// be called to check what error occurred during iteration.
//
// For example:
//
//    pipeline := []bson.M{}
//
//    changeStream := collection.Watch(pipeline, ChangeStreamOptions{})
//    for changeStream.Next(&changeDoc) {
//        fmt.Printf("Change: %v\n", changeDoc)
//    }
//
//    if err := changeStream.Close(); err != nil {
//        return err
//    }
//
// If the pipeline used removes the _id field from the result, Next will error
// because the _id field is needed to resume iteration when an error occurs.
//
func (changeStream *ChangeStream) Next(result interface{}) bool {
	// the err field is being constantly overwritten and we don't want the user to
	// attempt to read it at this point so we lock.
	changeStream.m.Lock()

	defer changeStream.m.Unlock()

	// if we are in a state of error, then don't continue.
	if changeStream.err != nil {
		return false
	}

	var err error

	// attempt to fetch the change stream result.
	err = changeStream.fetchResultSet(result)
	if err == nil {
		return true
	}

	// check if the error is resumable
	if !isResumableError(err) {
		// error is not resumable, give up and return it to the user.
		changeStream.err = err
		return false
	}

	// try to resume.
	err = changeStream.resume()
	if err != nil {
		// we've not been able to successfully resume and should only try once,
		// so we give up.
		changeStream.err = err
		return false
	}

	// we've successfully resumed the changestream.
	// try to fetch the next result.
	err = changeStream.fetchResultSet(result)
	if err != nil {
		changeStream.err = err
		return false
	}

	return true
}

func constructChangeStreamPipeline(pipeline interface{},
	options ChangeStreamOptions) interface{} {
	pipelinev := reflect.ValueOf(pipeline)

	// ensure that the pipeline passed in is a slice.
	if pipelinev.Kind() != reflect.Slice {
		panic("pipeline argument must be a slice")
	}

	// construct the options to be used by the change notification
	// pipeline stage.
	changeStreamStageOptions := bson.M{}

	if options.FullDocument != "" {
		changeStreamStageOptions["fullDocument"] = options.FullDocument
	}
	if options.ResumeAfter != nil {
		changeStreamStageOptions["resumeAfter"] = options.ResumeAfter
	}
	changeStreamStage := bson.M{"$changeStream": changeStreamStageOptions}

	pipeOfInterfaces := make([]interface{}, pipelinev.Len()+1)

	// insert the change notification pipeline stage at the beginning of the
	// aggregation.
	pipeOfInterfaces[0] = changeStreamStage

	// convert the passed in slice to a slice of interfaces.
	for i := 0; i < pipelinev.Len(); i++ {
		pipeOfInterfaces[1+i] = pipelinev.Index(i).Addr().Interface()
	}
	var pipelineAsInterface interface{} = pipeOfInterfaces
	return pipelineAsInterface
}

func (changeStream *ChangeStream) resume() error {
	// copy the information for the new socket.

	// Copy() destroys the sockets currently associated with this session
	// so future uses will acquire a new socket against the newly selected DB.
	newSession := changeStream.iter.session.Copy()

	// fetch the cursor from the iterator and use it to run a killCursors
	// on the connection.
	cursorId := changeStream.iter.op.cursorId
	err := runKillCursorsOnSession(newSession, cursorId)
	if err != nil {
		return err
	}

	// change out the old connection to the database with the new connection.
	changeStream.collection.Database.Session = newSession

	// make a new pipeline containing the resume token.
	changeStreamPipeline := constructChangeStreamPipeline(changeStream.pipeline, changeStream.options)

	// generate the new iterator with the new connection.
	newPipe := changeStream.collection.Pipe(changeStreamPipeline)
	changeStream.iter = newPipe.Iter()
	changeStream.iter.isChangeStream = true

	return nil
}

// fetchResumeToken unmarshals the _id field from the document, setting an error
// on the changeStream if it is unable to.
func (changeStream *ChangeStream) fetchResumeToken(rawResult *bson.Raw) error {
	changeStreamResult := struct {
		ResumeToken *bson.Raw `bson:"_id,omitempty"`
	}{}

	err := rawResult.Unmarshal(&changeStreamResult)
	if err != nil {
		return err
	}

	if changeStreamResult.ResumeToken == nil {
		return fmt.Errorf("resume token missing from result")
	}

	changeStream.resumeToken = changeStreamResult.ResumeToken
	return nil
}

func (changeStream *ChangeStream) fetchResultSet(result interface{}) error {
	rawResult := bson.Raw{}

	// fetch the next set of documents from the cursor.
	gotNext := changeStream.iter.Next(&rawResult)

	err := changeStream.iter.Err()
	if err != nil {
		return err
	}

	if !gotNext && err == nil {
		// If the iter.Err() method returns nil despite us not getting a next batch,
		// it is becuase iter.Err() silences this case.
		return ErrNotFound
	}

	// grab the resumeToken from the results
	if err := changeStream.fetchResumeToken(&rawResult); err != nil {
		return err
	}

	// put the raw results into the data structure the user provided.
	if err := rawResult.Unmarshal(result); err != nil {
		return err
	}
	return nil
}

func isResumableError(err error) bool {
	_, isQueryError := err.(*QueryError)
	// if it is not a database error OR it is a database error,
	// but the error is a notMaster error
	return !isQueryError || isNotMasterError(err)
}

func runKillCursorsOnSession(session *Session, cursorId int64) error {
	socket, err := session.acquireSocket(true)
	if err != nil {
		return err
	}
	err = socket.Query(&killCursorsOp{[]int64{cursorId}})
	if err != nil {
		return err
	}
	socket.Release()

	return nil
}
