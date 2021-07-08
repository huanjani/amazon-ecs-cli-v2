// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

/* jshint node: true */
/*jshint esversion: 8 */

"use strict";

const AWS = require('aws-sdk');

const ERR_NAME_INVALID_REQUEST = "InvalidRequestException";
const DOMAIN_STATUS_PENDING_VERIFICATION = "pending_certificate_dns_validation";
const DOMAIN_STATUS_ACTIVE = "active";
const ATTEMPTS_WAIT_FOR_PENDING = 10;
const ATTEMPTS_WAIT_FOR_ACTIVE = 12;

let defaultSleep = function (ms) {
    return new Promise((resolve) => setTimeout(resolve, ms));
};
let sleep = defaultSleep;
let appRoute53Client, appRunnerClient, appHostedZoneID;

/**
 * Upload a CloudFormation response object to S3.
 *
 * @param {object} event the Lambda event payload received by the handler function
 * @param {object} context the Lambda context received by the handler function
 * @param {string} responseStatus the response status, either 'SUCCESS' or 'FAILED'
 * @param {string} physicalResourceId CloudFormation physical resource ID
 * @param {object} [responseData] arbitrary response data object
 * @param {string} [reason] reason for failure, if any, to convey to the user
 * @returns {Promise} Promise that is resolved on success, or rejected on connection error or HTTP error response
 */
function report (
    event,
    context,
    responseStatus,
    physicalResourceId,
    responseData,
    reason
) {
    return new Promise((resolve, reject) => {
        const https = require("https");
        const { URL } = require("url");

        let reasonWithLogInfo = `${reason} (Log: ${context.logGroupName}/${context.logStreamName})`;
        var responseBody = JSON.stringify({
            Status: responseStatus,
            Reason: reasonWithLogInfo,
            PhysicalResourceId: physicalResourceId || context.logStreamName,
            StackId: event.StackId,
            RequestId: event.RequestId,
            LogicalResourceId: event.LogicalResourceId,
            Data: responseData,
        });

        const parsedUrl = new URL(event.ResponseURL);
        const options = {
            hostname: parsedUrl.hostname,
            port: 443,
            path: parsedUrl.pathname + parsedUrl.search,
            method: "PUT",
            headers: {
                "Content-Type": "",
                "Content-Length": responseBody.length,
            },
        };

        https
            .request(options)
            .on("error", reject)
            .on("response", (res) => {
                res.resume();
                if (res.statusCode >= 400) {
                    reject(new Error(`Error ${res.statusCode}: ${res.statusMessage}`));
                } else {
                    resolve();
                }
            })
            .end(responseBody, "utf8");
    });
}

exports.handler = async function (event, context) {
    const props = event.ResourceProperties;
    const [serviceARN, appDNSRole, customDomain] = [props.ServiceARN, props.AppDNSRole, props.CustomDomain,];
    appHostedZoneID = props.HostedZoneID;
    const physicalResourceID = `/associate-domain-app-runner/${customDomain}`;
    let handler = async function () {
        // Configure clients.
        appRoute53Client = new AWS.Route53({
            credentials: new AWS.ChainableTemporaryCredentials({
                params: { RoleArn: appDNSRole, },
                masterCredentials: new AWS.EnvironmentCredentials("AWS"),
            }),
        });
        appRunnerClient = new AWS.AppRunner();

        switch (event.RequestType) {
            case "Create":
                await addCustomDomain(serviceARN, customDomain);
                await waitForCustomDomainToBeActive(serviceARN, customDomain);
                break;
            case "Update":
            case "Delete":
                throw new Error("not yet implemented");
            default:
                throw new Error(`Unsupported request type ${event.RequestType}`);
        }
    };

    try {
        await Promise.race([exports.deadlineExpired(), handler(),]);
        await report(event, context, "SUCCESS", physicalResourceID);
    } catch (err) {
        if (err.name === ERR_NAME_INVALID_REQUEST && err.message.includes(`${customDomain} is already associated with`)) {
            await report(event, context, "SUCCESS", physicalResourceID);
            return;
        }
        console.log(`Caught error for service ${serviceARN}: ${err.message}`);
        await report(event, context, "FAILED", physicalResourceID, null, err.message);
    }
};

exports.deadlineExpired = function () {
    return new Promise(function (resolve, reject) {
        setTimeout(
            reject,
            14 * 60 * 1000 + 30 * 1000 /* 14.5 minutes*/,
            new Error("Lambda took longer than 14.5 minutes to update environment")
        );
    });
};

/**
 * Validate certificates of the custom domain for the service by upserting validation records.
 * Errors are not handled and are directly passed to the caller.
 *
 * @param {string} serviceARN ARN of the service that the custom domain applies to.
 * @param {string} customDomainName the custom domain name.
 */
async function addCustomDomain(serviceARN, customDomainName) {
    const data = await appRunnerClient.associateCustomDomain({
        DomainName: customDomainName,
        ServiceArn: serviceARN,
    }).promise();

    return Promise.all([
        updateCNAMERecordAndWait(customDomainName, data.DNSTarget, appHostedZoneID, "UPSERT"), // Upsert the record that maps `customDomainName` to the DNS of the app runner service.
        validateCertForDomain(serviceARN, customDomainName),
    ]);
}

/**
 * Wait for the custom domain to be ACTIVE.
 * @param {string} serviceARN the service to which the domain is added.
 * @param {string} customDomainName the domain name.
 */
async function waitForCustomDomainToBeActive(serviceARN, customDomainName) {
    let i;
    for (i = 0; i < ATTEMPTS_WAIT_FOR_ACTIVE; i++) {
        const data = await appRunnerClient.describeCustomDomains({
            ServiceArn: serviceARN,
        }).promise().catch(err => {
            throw new Error(`wait for domain ${customDomainName} to be active: ` + err.message);
        });

        let domain;
        for (const d of data.CustomDomains) {
            if (d.DomainName === customDomainName) {
                domain = d;
                break;
            }
        }

        if (!domain) {
            throw new Error(`wait for domain ${customDomainName} to be active: domain ${customDomainName} is not associated`);
        }

        if (domain.Status !== DOMAIN_STATUS_ACTIVE) {
            // Exponential backoff with jitter based on 200ms base
            // component of backoff fixed to ensure minimum total wait time on
            // slow targets.
            const base = Math.pow(2, i);
            await sleep(Math.random() * base * 50 + base * 150);
            continue;
        }
        return;
    }

    if (i === ATTEMPTS_WAIT_FOR_ACTIVE) {
        console.log("Fail to wait for the domain status to become ACTIVE. It usually takes a long time to validate domain and can be longer than the 15 minutes duration for which a Lambda function can run at most. Try associating the domain manually.");
        throw new Error(`fail to wait for domain ${customDomainName} to become ${DOMAIN_STATUS_ACTIVE}`);
    }
}

/**
 * Get information about domain.
 * @param {string} serviceARN
 * @param {string} domainName
 * @returns {object} CustomDomain object that contains information such as DomainName, Status, CertificateValidationRecords, etc.
 * @throws error if domain is not found in service.
 */
async function getDomainInfo(serviceARN, domainName) {
    let describeCustomDomainsInput = {ServiceArn: serviceARN,};
    while (true) {
        const resp = await appRunnerClient.describeCustomDomains(describeCustomDomainsInput).promise();

        for (const d of resp.CustomDomains) {
            if (d.DomainName === domainName) {
                return d;
            }
        }

        if (!resp.NextToken) {
            throw new Error(`domain ${domainName} is not associated`);
        }
        describeCustomDomainsInput.NextToken = resp.NextToken;
    }
}

/**
 * Validate certificates of the custom domain for the service by upserting validation records.
 *
 * @param {string} serviceARN ARN of the service that the custom domain applies to.
 * @param {string} domainName the custom domain name.
 * @throws wrapped error.
 */
async function validateCertForDomain(serviceARN, domainName) {
    let i, lastDomainStatus;
    for (i = 0; i < ATTEMPTS_WAIT_FOR_PENDING; i++){
        const domain = await getDomainInfo(serviceARN, domainName).catch(err => {
            throw new Error(`update validation records for domain ${domainName}: ` + err.message);
        });

        lastDomainStatus = domain.Status;
        if (lastDomainStatus !== DOMAIN_STATUS_PENDING_VERIFICATION) {
            await sleep(3000);
            continue;
        }
        // Upsert all records needed for certificate validation.
        const records = domain.CertificateValidationRecords;
        for (const record of records) {
            await updateCNAMERecordAndWait(record.Name, record.Value, appHostedZoneID, "UPSERT").catch(err => {
                throw new Error(`update validation records for domain ${domainName}: ` + err.message);
            });
        }
        break;
    }

    if (i === ATTEMPTS_WAIT_FOR_PENDING) {
        throw new Error(`update validation records for domain ${domainName}: fail to wait for state ${DOMAIN_STATUS_PENDING_VERIFICATION}, stuck in ${lastDomainStatus}`);
    }
}

/**
 * Upserts a CNAME record and wait for the change to have taken place.
 *
 * @param {string} recordName the name of the record
 * @param {string} recordValue the value of the record
 * @param {string} hostedZoneID the ID of the hosted zone into which the record needs to be upserted.
 * @param {string} action the action to perform; can be "CREATE", "DELETE", or "UPSERT".
 * @throws wrapped error.
 */
async function updateCNAMERecordAndWait(recordName, recordValue, hostedZoneID, action) {
    let params = {
        ChangeBatch: {
            Changes: [
                {
                    Action: action,
                    ResourceRecordSet: {
                        Name: recordName,
                        Type: "CNAME",
                        TTL: 60,
                        ResourceRecords: [
                            {
                                Value: recordValue,
                            },
                        ],
                    },
                },
            ],
        },
        HostedZoneId: hostedZoneID,
    };

     const data = await appRoute53Client.changeResourceRecordSets(params).promise().catch((err) => {
        throw new Error(`update record ${recordName}: ` + err.message);
    });

     await appRoute53Client.waitFor('resourceRecordSetsChanged', {
         // Wait up to 5 minutes
         $waiter: {
             delay: 30,
             maxAttempts: 10,
         },
         Id: data.ChangeInfo.Id,
     }).promise().catch((err) => {
         throw new Error(`update record ${recordName}: wait for record sets change for ${recordName}: ` + err.message);
     });
}

exports.domainStatusPendingVerification = DOMAIN_STATUS_PENDING_VERIFICATION;
exports.waitForDomainStatusPendingAttempts = ATTEMPTS_WAIT_FOR_PENDING;
exports.waitForDomainStatusActiveAttempts = ATTEMPTS_WAIT_FOR_ACTIVE;
exports.withSleep = function (s) {
    sleep = s;
};
exports.reset = function () {
    sleep = defaultSleep;

};
exports.withDeadlineExpired = function (d) {
    exports.deadlineExpired = d;
};
