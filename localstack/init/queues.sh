#!/bin/sh
set -eu
awslocal sqs create-queue --queue-name wager-transactions-dlq.fifo --attributes FifoQueue=true,ContentBasedDeduplication=false
DLQ_URL=$(awslocal sqs get-queue-url --queue-name wager-transactions-dlq.fifo --query QueueUrl --output text)
DLQ_ARN=$(awslocal sqs get-queue-attributes --queue-url "$DLQ_URL" --attribute-names QueueArn --query 'Attributes.QueueArn' --output text)
export DLQ_ARN
ATTRIBUTES=$(python3 - <<'PY'
import json, os
print(json.dumps({"FifoQueue":"true","ContentBasedDeduplication":"false","VisibilityTimeout":"30","ReceiveMessageWaitTimeSeconds":"10","RedrivePolicy":json.dumps({"deadLetterTargetArn":os.environ["DLQ_ARN"],"maxReceiveCount":"5"})}))
PY
)
awslocal sqs create-queue --queue-name wager-transactions.fifo --attributes "$ATTRIBUTES"
awslocal sqs create-queue --queue-name wager-events.fifo --attributes FifoQueue=true,ContentBasedDeduplication=false

INPUT_URL=$(awslocal sqs get-queue-url --queue-name wager-transactions.fifo --query QueueUrl --output text)
EVENT_URL=$(awslocal sqs get-queue-url --queue-name wager-events.fifo --query QueueUrl --output text)
INPUT_ARN=$(awslocal sqs get-queue-attributes --queue-url "$INPUT_URL" --attribute-names QueueArn --query 'Attributes.QueueArn' --output text)
EVENT_ARN=$(awslocal sqs get-queue-attributes --queue-url "$EVENT_URL" --attribute-names QueueArn --query 'Attributes.QueueArn' --output text)
export INPUT_ARN EVENT_ARN

python3 - <<'PY' > /tmp/wager-producer-policy.json
import json, os
print(json.dumps({"Version":"2012-10-17","Statement":[
  {"Effect":"Allow","Action":["sqs:GetQueueUrl","sqs:SendMessage"],"Resource":os.environ["INPUT_ARN"]}
]}))
PY
python3 - <<'PY' > /tmp/wager-consumer-policy.json
import json, os
print(json.dumps({"Version":"2012-10-17","Statement":[
  {"Effect":"Allow","Action":["sqs:GetQueueUrl","sqs:GetQueueAttributes","sqs:ReceiveMessage","sqs:DeleteMessage","sqs:ChangeMessageVisibility"],"Resource":os.environ["INPUT_ARN"]},
  {"Effect":"Allow","Action":["sqs:GetQueueUrl","sqs:GetQueueAttributes","sqs:SendMessage"],"Resource":os.environ["DLQ_ARN"]},
  {"Effect":"Allow","Action":["sqs:GetQueueUrl","sqs:SendMessage"],"Resource":os.environ["EVENT_ARN"]}
]}))
PY

for role in producer consumer; do
  user="wager-$role"
  awslocal iam get-user --user-name "$user" >/dev/null 2>&1 || awslocal iam create-user --user-name "$user" >/dev/null
  awslocal iam put-user-policy --user-name "$user" --policy-name "wager-$role-minimum" --policy-document "file:///tmp/wager-$role-policy.json"
  for old_key in $(awslocal iam list-access-keys --user-name "$user" --query 'AccessKeyMetadata[].AccessKeyId' --output text); do
    awslocal iam delete-access-key --user-name "$user" --access-key-id "$old_key"
  done
  mkdir -p "/var/run/wager-credentials/$role"
  awslocal iam create-access-key --user-name "$user" | python3 -c 'import json,sys; k=json.load(sys.stdin)["AccessKey"]; role=sys.argv[1]; path="/var/run/wager-credentials/"+role+"/credentials"; open(path,"w").write("["+role+"]\naws_access_key_id="+k["AccessKeyId"]+"\naws_secret_access_key="+k["SecretAccessKey"]+"\n")' "$role"
  chmod 0644 "/var/run/wager-credentials/$role/credentials"
done
