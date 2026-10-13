package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// SQSConfig descreve a conexão. Endpoint vazio usa a AWS real e a cadeia padrão de
// credenciais; com Endpoint (LocalStack) usam-se as credenciais estáticas informadas.
type SQSConfig struct {
	Endpoint  string
	Region    string
	AccessKey string
	SecretKey string
}

// NewSQSClient cria o cliente do SDK.
func NewSQSClient(ctx context.Context, c SQSConfig) (*sqs.Client, error) {
	opts := []func(*config.LoadOptions) error{config.WithRegion(c.Region)}
	if c.AccessKey != "" {
		opts = append(opts, config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(c.AccessKey, c.SecretKey, "")))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return sqs.NewFromConfig(cfg, func(o *sqs.Options) {
		if c.Endpoint != "" {
			o.BaseEndpoint = aws.String(c.Endpoint)
		}
	}), nil
}

// SQS é uma Queue sobre uma fila SQS FIFO.
type SQS struct {
	client *sqs.Client
	url    string
	arn    string
}

// URL devolve a URL da fila.
func (q *SQS) URL() string { return q.url }

// EnsureFIFOQueue cria a fila FIFO (se não existir) e devolve o adaptador. CreateQueue é
// idempotente quando os atributos coincidem.
func EnsureFIFOQueue(ctx context.Context, client *sqs.Client, name string, visibility time.Duration, redrive map[string]any) (*SQS, error) {
	attrs := map[string]string{
		"FifoQueue":                 "true",
		"ContentBasedDeduplication": "false",
		"VisibilityTimeout":         strconv.Itoa(int(visibility.Seconds())),
	}
	if redrive != nil {
		b, err := json.Marshal(redrive)
		if err != nil {
			return nil, err
		}
		attrs["RedrivePolicy"] = string(b)
	}
	out, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(name), Attributes: attrs})
	if err != nil {
		return nil, fmt.Errorf("create queue %s: %w", name, err)
	}
	q := &SQS{client: client, url: aws.ToString(out.QueueUrl)}
	ga, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: out.QueueUrl, AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn}})
	if err != nil {
		return nil, fmt.Errorf("queue attributes %s: %w", name, err)
	}
	q.arn = ga.Attributes[string(types.QueueAttributeNameQueueArn)]
	return q, nil
}

// EnsureFIFOWithDLQ provisiona a DLQ e a fila principal com redrive (maxReceive entregas).
func EnsureFIFOWithDLQ(ctx context.Context, client *sqs.Client, name, dlqName string, visibility time.Duration, maxReceive int) (main, dlq *SQS, err error) {
	dlq, err = EnsureFIFOQueue(ctx, client, dlqName, visibility, nil)
	if err != nil {
		return nil, nil, err
	}
	main, err = EnsureFIFOQueue(ctx, client, name, visibility, map[string]any{
		"deadLetterTargetArn": dlq.arn, "maxReceiveCount": maxReceive,
	})
	return main, dlq, err
}

func (q *SQS) Send(ctx context.Context, in SendInput) error {
	_, err := q.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl: aws.String(q.url), MessageBody: aws.String(in.Body),
		MessageGroupId: aws.String(in.GroupID), MessageDeduplicationId: aws.String(in.DeduplicationID),
	})
	return err
}

func (q *SQS) Receive(ctx context.Context, max int, wait time.Duration) ([]Message, error) {
	out, err := q.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl: aws.String(q.url), MaxNumberOfMessages: int32(max), WaitTimeSeconds: int32(wait.Seconds()),
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{
			types.MessageSystemAttributeNameApproximateReceiveCount, types.MessageSystemAttributeNameMessageGroupId},
	})
	if err != nil {
		return nil, err
	}
	msgs := make([]Message, 0, len(out.Messages))
	for _, m := range out.Messages {
		n, _ := strconv.Atoi(m.Attributes[string(types.MessageSystemAttributeNameApproximateReceiveCount)])
		msgs = append(msgs, Message{
			ID: aws.ToString(m.MessageId), Body: aws.ToString(m.Body), ReceiptHandle: aws.ToString(m.ReceiptHandle),
			ReceiveCount: n, GroupID: m.Attributes[string(types.MessageSystemAttributeNameMessageGroupId)],
		})
	}
	return msgs, nil
}

func (q *SQS) Delete(ctx context.Context, receiptHandle string) error {
	_, err := q.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(q.url), ReceiptHandle: aws.String(receiptHandle)})
	return err
}

func (q *SQS) Release(ctx context.Context, receiptHandle string, delay time.Duration) error {
	_, err := q.client.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl: aws.String(q.url), ReceiptHandle: aws.String(receiptHandle), VisibilityTimeout: int32(delay.Seconds()),
	})
	return err
}

func (q *SQS) Ping(ctx context.Context) error {
	if q.url == "" {
		return errors.New("queue not provisioned")
	}
	_, err := q.client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(q.url), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn}})
	return err
}

// Approximate devolve contagens aproximadas (o SQS é eventualmente consistente): mensagens
// visíveis, em voo e em atraso. Útil em testes e diagnóstico.
func (q *SQS) Approximate(ctx context.Context) (visible, inFlight, delayed int, err error) {
	out, err := q.client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl: aws.String(q.url),
		AttributeNames: []types.QueueAttributeName{
			types.QueueAttributeNameApproximateNumberOfMessages,
			types.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
			types.QueueAttributeNameApproximateNumberOfMessagesDelayed,
		},
	})
	if err != nil {
		return 0, 0, 0, err
	}
	visible, _ = strconv.Atoi(out.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessages)])
	inFlight, _ = strconv.Atoi(out.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessagesNotVisible)])
	delayed, _ = strconv.Atoi(out.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessagesDelayed)])
	return visible, inFlight, delayed, nil
}
